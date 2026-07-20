package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/evalx"
	"github.com/llmrelay/relay/internal/pricing"
	"github.com/llmrelay/relay/internal/provider"
	"github.com/llmrelay/relay/internal/server"
	"github.com/llmrelay/relay/internal/smart"
	"github.com/llmrelay/relay/internal/store"
)

// runTrain builds/updates the tier-2 reference set from relay's own logs
// (DESIGN §0.4). Three opt-in label sources: implicit signals (free),
// replay+judge (spends tokens — estimate-before-run is binding), and the
// /v1/feedback scores. Works from log_prompts: embeddings without raw
// bodies (stored vectors carry the training value).
func runTrain(args []string) error {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default: discovery + env)")
	dbPath := fs.String("db", "", "request-log path (default from config)")
	refsPath := fs.String("refs", "", "reference-set path (default from config)")
	embedderSpec := fs.String("embedder", "", "provider/model embedder (default routing.smart.embeddings)")
	since := fs.Duration("since", 30*24*time.Hour, "log window to mine")
	implicit := fs.Bool("implicit", false, "mine implicit signals + feedback scores from the log")
	replayN := fs.Int("replay", 0, "replay+judge: sample N logged prompts (requires log_prompts: full)")
	judgeSpec := fs.String("judge", "", "provider/model that scores replayed responses")
	dryRun := fs.Bool("dry-run", false, "print the projected spend and stop before any API call")
	yes := fs.Bool("yes", false, "confirm spending without a prompt (for non-interactive runs)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var cfg *config.Config
	if path := config.Find(*cfgPath); path != "" {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return err
		}
	} else {
		cfg = config.Sniff()
	}
	if *embedderSpec == "" {
		*embedderSpec = cfg.Routing.Smart.Embeddings
	}
	if *embedderSpec == "" {
		return fmt.Errorf("no embedder: set routing.smart.embeddings or pass --embedder provider/model")
	}
	if *refsPath == "" {
		if *refsPath = cfg.Routing.Smart.Reference; *refsPath == "" {
			*refsPath = config.DefaultSmartRefsPath()
		}
	}
	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		return err
	}
	embedFn, err := bindEmbedder(rt, *embedderSpec)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Start from the persisted store when it matches this embedder,
	// otherwise from freshly embedded seed texts.
	refStore, err := smart.LoadRefStore(*refsPath)
	if err != nil || refStore.Embedder != *embedderSpec {
		seeds, err := smart.SeedRefs()
		if err != nil {
			return err
		}
		fmt.Printf("building reference set from %d seed texts (embedder %s)\n", len(seeds), *embedderSpec)
		texts := make([]string, len(seeds))
		for i, r := range seeds {
			texts[i] = r.Text
		}
		vecs, err := embedFn(ctx, texts)
		if err != nil {
			return fmt.Errorf("embedding seed texts: %w", err)
		}
		for i := range seeds {
			seeds[i].Vector = vecs[i]
		}
		refStore = &smart.RefStore{Embedder: *embedderSpec, Refs: seeds}
	}
	have := map[string]bool{}
	for _, r := range refStore.Refs {
		have[r.ID] = true
	}

	if *implicit || *replayN > 0 {
		if *dbPath == "" {
			if *dbPath = cfg.Logging.DB; *dbPath == "" {
				*dbPath = config.DefaultDBPath()
			}
		}
		st, err := store.Open(*dbPath)
		if err != nil {
			return fmt.Errorf("opening request log: %w", err)
		}
		defer st.Close()

		if *implicit {
			added, skipped, err := mineImplicit(ctx, st.DB(), refStore, have, time.Now().Add(-*since), embedFn)
			if err != nil {
				return err
			}
			fmt.Printf("implicit signals: %d reference points added, %d rows skipped (no vector and no body to embed)\n", added, skipped)
		}
		if *replayN > 0 {
			if *judgeSpec == "" {
				return fmt.Errorf("--replay requires --judge provider/model")
			}
			if err := replayAndJudge(ctx, rt, st.DB(), refStore, have, *replayN, *judgeSpec, *since, *dryRun, *yes, embedFn); err != nil {
				return err
			}
			if *dryRun {
				return nil // estimate printed; nothing spent, nothing to save
			}
		}
	}

	if err := smart.SaveRefStore(*refsPath, refStore); err != nil {
		return err
	}
	fmt.Printf("reference set: %d points (embedder %s) → %s\n", len(refStore.Refs), *embedderSpec, *refsPath)
	return nil
}

func bindEmbedder(rt *server.Runtime, spec string) (smart.EmbedFunc, error) {
	provName, model, ok := strings.Cut(spec, "/")
	if !ok {
		return nil, fmt.Errorf("embedder %q is not provider/model", spec)
	}
	p, exists := rt.Providers[provName]
	if !exists {
		return nil, fmt.Errorf("embedder provider %q not configured", provName)
	}
	emb, okE := p.(provider.Embedder)
	if !okE {
		return nil, fmt.Errorf("provider %q has no embeddings API", provName)
	}
	return func(ctx context.Context, texts []string) ([][]float32, error) {
		resp, err := emb.Embed(ctx, &provider.EmbedRequest{Model: model, Input: texts})
		if err != nil {
			return nil, err
		}
		return resp.Vectors, nil
	}, nil
}

// mineImplicit turns logged failure/feedback signals into reference
// points. Stored prompt embeddings are used as-is (log_prompts:
// embeddings — no raw text needed); rows with only a body are embedded
// now; rows with neither are skipped and counted.
func mineImplicit(ctx context.Context, db *sql.DB, refStore *smart.RefStore, have map[string]bool, cutoff time.Time, embed smart.EmbedFunc) (added, skipped int, err error) {
	rows, err := db.Query(`
		SELECT id, prompt_embedding, prompt_body, feedback_score, attempts, status
		FROM requests
		WHERE ts >= ? AND provider != ''
		  AND (feedback_score IS NOT NULL OR attempts > 1 OR status >= 500)`,
		cutoff.UnixMilli())
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var blob []byte
		var body sql.NullString
		var score sql.NullFloat64
		var attempts, status sql.NullInt64
		if err := rows.Scan(&id, &blob, &body, &score, &attempts, &status); err != nil {
			return added, skipped, err
		}
		refID := "implicit-" + id
		if have[refID] {
			continue
		}
		// Documented heuristics — weak but free (DESIGN §0.4):
		// a failover or 5xx means the first-choice model wasn't enough;
		// explicit low feedback means the served answer wasn't either.
		difficulty, why := 0.0, ""
		switch {
		case score.Valid && score.Float64 <= 0.4:
			difficulty, why = 0.75, "low feedback score"
		case score.Valid && score.Float64 >= 0.8:
			difficulty, why = 0.30, "high feedback score"
		case attempts.Int64 > 1 || status.Int64 >= 500:
			difficulty, why = 0.70, "failover/5xx"
		default:
			continue
		}
		vec := store.DecodeVector(blob)
		if vec == nil && body.Valid && body.String != "" {
			vecs, err := embed(ctx, []string{body.String})
			if err != nil || len(vecs) != 1 {
				skipped++
				continue
			}
			vec = vecs[0]
		}
		if vec == nil {
			skipped++
			continue
		}
		refStore.Refs = append(refStore.Refs, smart.Ref{
			ID: refID, Difficulty: difficulty, Domain: "traffic",
			Source: "implicit(" + why + ")", Vector: vec,
		})
		have[refID] = true
		added++
	}
	return added, skipped, rows.Err()
}

// replayAndJudge is the honest offline-training loop: sample logged
// prompts, replay against the cheap candidate, score with the judge, and
// label difficulty by how well the cheap model did. Binding: the spend
// estimate prints first, and nothing is called without --dry-run being
// dropped AND explicit confirmation.
func replayAndJudge(ctx context.Context, rt *server.Runtime, db *sql.DB, refStore *smart.RefStore, have map[string]bool, n int, judgeSpec string, since time.Duration, dryRun, yes bool, embed smart.EmbedFunc) error {
	rows, err := db.Query(`
		SELECT id, prompt_body FROM requests
		WHERE ts >= ? AND prompt_body IS NOT NULL AND prompt_body != '' AND provider != ''
		ORDER BY ts DESC LIMIT ?`, time.Now().Add(-since).UnixMilli(), n)
	if err != nil {
		return err
	}
	defer rows.Close()
	type sample struct{ id, prompt string }
	var samples []sample
	for rows.Next() {
		var s sample
		var rawBody string
		if err := rows.Scan(&s.id, &rawBody); err != nil {
			return err
		}
		// prompt_body is the raw inbound JSON (either dialect); replay the
		// extracted user text, not the wire envelope.
		if s.prompt = extractUserText(rawBody); s.prompt == "" {
			continue
		}
		samples = append(samples, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(samples) == 0 {
		return fmt.Errorf("no replayable prompts: --replay needs logging.log_prompts: full (embeddings-tier logs carry vectors, not text)")
	}

	cheapSpec := rt.Config.Routing.Smart.Easy
	judgeProv, judgeModel, ok := strings.Cut(judgeSpec, "/")
	if !ok {
		return fmt.Errorf("--judge %q is not provider/model", judgeSpec)
	}
	cheapProv, cheapModel, ok := strings.Cut(cheapSpec, "/")
	if !ok {
		return fmt.Errorf("routing.smart.easy %q must be provider/model for replay (aliases not yet supported here)", cheapSpec)
	}

	// Estimate BEFORE anything is called (DESIGN §0.4, binding).
	reg, err := pricing.Load()
	if err != nil {
		return err
	}
	const outEst, judgeOutEst = 256, 8
	var costEst float64
	var tokensEst int
	for _, s := range samples {
		tok := evalx.EstTokensIn(s.prompt)
		tokensEst += tok
		if in, out, ok := reg.Price([]string{cheapProv}, cheapModel); ok {
			costEst += (float64(tok)*in + outEst*out) / 1e6
		}
		if in, out, ok := reg.Price([]string{judgeProv}, judgeModel); ok {
			costEst += (float64(tok+outEst)*in + judgeOutEst*out) / 1e6
		}
	}
	fmt.Printf("replay+judge plan: %d prompts (~%d input tokens) × candidate %s + judge %s\n",
		len(samples), tokensEst, cheapSpec, judgeSpec)
	fmt.Printf("projected spend: $%.4f (candidate + judge, prices from registry %s; unpriced models excluded from the estimate)\n", costEst, reg.Version)
	if dryRun {
		fmt.Println("--dry-run: stopping before any API call")
		return nil
	}
	if !yes {
		fmt.Print("proceed and spend? type 'yes' to continue: ")
		sc := bufio.NewScanner(os.Stdin)
		if !sc.Scan() || strings.TrimSpace(sc.Text()) != "yes" {
			return fmt.Errorf("not confirmed; nothing was spent")
		}
	}

	cheap, okP := rt.Providers[cheapProv]
	if !okP {
		return fmt.Errorf("candidate provider %q not configured", cheapProv)
	}
	judge, okJ := rt.Providers[judgeProv]
	if !okJ {
		return fmt.Errorf("judge provider %q not configured", judgeProv)
	}

	added := 0
	for _, s := range samples {
		refID := "judge-" + s.id
		if have[refID] {
			continue
		}
		answer, err := completeText(ctx, cheap, cheapModel, s.prompt)
		if err != nil {
			fmt.Printf("  %s: candidate failed (%v) — labeling hard\n", s.id, err)
			answer = ""
		}
		score := 0.0
		if answer != "" {
			score, err = judgeScore(ctx, judge, judgeModel, s.prompt, answer)
			if err != nil {
				fmt.Printf("  %s: judge failed (%v) — skipping\n", s.id, err)
				continue
			}
		}
		vecs, err := embed(ctx, []string{s.prompt})
		if err != nil || len(vecs) != 1 {
			fmt.Printf("  %s: embed failed (%v) — skipping\n", s.id, err)
			continue
		}
		// Difficulty = how badly the cheap model did.
		refStore.Refs = append(refStore.Refs, smart.Ref{
			ID: refID, Difficulty: 1 - score, Domain: "traffic",
			Source: fmt.Sprintf("judge(%s scored %.2f)", cheapSpec, score), Vector: vecs[0],
		})
		have[refID] = true
		added++
	}
	fmt.Printf("replay+judge: %d reference points added\n", added)
	return nil
}

// extractUserText pulls the last user message's text out of a logged
// request body. Both dialects share the messages shape; content is a
// string or an array of typed parts.
func extractUserText(rawBody string) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(rawBody), &req); err != nil {
		return ""
	}
	last := ""
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			last = s
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				b.WriteString(p.Text)
			}
			last = b.String()
		}
	}
	return strings.TrimSpace(last)
}

func completeText(ctx context.Context, p provider.Provider, model, prompt string) (string, error) {
	text, _, err := completeTextN(ctx, p, model, prompt, 0)
	return text, err
}

// completeTextN is completeText with a max-token cap and usage reporting
// (the live-judged eval prices real usage).
func completeTextN(ctx context.Context, p provider.Provider, model, prompt string, maxTokens int) (string, core.Usage, error) {
	req := &core.Request{
		Model:    model,
		Messages: []core.Message{{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: prompt}}}},
	}
	if maxTokens > 0 {
		req.MaxTokens = &maxTokens
	}
	resp, err := p.Complete(ctx, req)
	if err != nil {
		return "", core.Usage{}, err
	}
	var b strings.Builder
	for _, part := range resp.Choices[0].Parts {
		if tp, ok := part.(core.TextPart); ok {
			b.WriteString(tp.Text)
		}
	}
	return b.String(), resp.Usage, nil
}

func judgeScore(ctx context.Context, p provider.Provider, model, prompt, answer string) (float64, error) {
	judgePrompt := fmt.Sprintf(
		"Rate the RESPONSE to the PROMPT for correctness and helpfulness on a 0-10 scale. Reply with ONLY the number.\n\nPROMPT:\n%s\n\nRESPONSE:\n%s",
		prompt, answer)
	out, err := completeText(ctx, p, model, judgePrompt)
	if err != nil {
		return 0, err
	}
	if score, ok := parseJudgeScore(out); ok {
		return score, nil
	}
	return 0, fmt.Errorf("judge replied %q, not parseable as a 0-10 score", out)
}

var trailingScoreRe = regexp.MustCompile(`(?:^|[\s>*:.\x60])(\d+(?:\.\d+)?)\s*$`)

// parseJudgeScore applies deterministic parsing attempts, in order: the
// whole reply as a number, then a trailing standalone number — judges
// sometimes prepend commentary despite "reply with ONLY the number"
// (2026-07-20: 6/147 replies did exactly that and were wrongly scored 0
// until corrected; see assets/eval verdict corrections log).
func parseJudgeScore(out string) (float64, bool) {
	trimmed := strings.TrimSpace(out)
	var score float64
	if _, err := fmt.Sscanf(trimmed, "%g", &score); err == nil {
		return clampScore(score), true
	}
	if m := trailingScoreRe.FindStringSubmatch(trimmed); m != nil {
		if _, err := fmt.Sscanf(m[1], "%g", &score); err == nil && score >= 0 && score <= 10 {
			return clampScore(score), true
		}
	}
	return 0, false
}

func clampScore(s float64) float64 {
	if s < 0 {
		s = 0
	}
	if s > 10 {
		s = 10
	}
	return s / 10
}
