package main

import (
	"context"
	"flag"
	"fmt"
	"html"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/pricing"
)

// runCompare fans one prompt (or a file of prompts) to N models and
// reports output, cost, latency, and TTFT side by side. Reuses the
// provider adapters, streaming plumbing, and pricing registry.
func runCompare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	models := fs.String("models", "", "comma-separated provider/model list (required)")
	file := fs.String("file", "", "file with one prompt per line (instead of a prompt argument)")
	maxTokens := fs.Int("max-tokens", 300, "completion cap per model")
	htmlPath := fs.String("html", "", "also write a self-contained HTML report")
	cfgPath := fs.String("config", "", "config file for provider credentials (default: discovery + env)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *models == "" {
		return fmt.Errorf("--models is required, e.g. --models gemini/gemini-3.1-flash-lite,anthropic/claude-haiku-4-5")
	}
	var prompts []string
	if *file != "" {
		raw, err := os.ReadFile(*file)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line = strings.TrimSpace(line); line != "" {
				prompts = append(prompts, line)
			}
		}
	}
	if rest := fs.Args(); len(rest) > 0 {
		prompts = append(prompts, strings.Join(rest, " "))
	}
	if len(prompts) == 0 {
		return fmt.Errorf("no prompts: pass one as an argument or use --file")
	}

	rt, err := evalRuntime(*cfgPath)
	if err != nil {
		return err
	}
	reg, err := pricing.Load()
	if err != nil {
		return err
	}
	specs := strings.Split(*models, ",")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	type cell struct {
		Spec      string
		TTFTMS    int64
		TotalMS   int64
		TokensIn  int
		TokensOut int
		CostUSD   *float64
		Output    string
		Err       string
	}
	grid := make([][]cell, len(prompts))

	for pi, prompt := range prompts {
		grid[pi] = make([]cell, len(specs))
		var wg sync.WaitGroup
		for si, spec := range specs {
			wg.Add(1)
			go func(si int, spec string) {
				defer wg.Done()
				c := cell{Spec: spec}
				defer func() { grid[pi][si] = c }()
				provName, model, ok := strings.Cut(strings.TrimSpace(spec), "/")
				if !ok {
					c.Err = "not provider/model"
					return
				}
				p, exists := rt.Providers[provName]
				if !exists {
					c.Err = fmt.Sprintf("provider %q not configured (missing key?)", provName)
					return
				}
				mt := *maxTokens
				req := &core.Request{
					Model:     model,
					MaxTokens: &mt,
					Stream:    true,
					Messages:  []core.Message{{Role: core.RoleUser, Parts: []core.Part{core.TextPart{Text: prompt}}}},
				}
				start := time.Now()
				st, err := p.Stream(ctx, req)
				if err != nil {
					c.Err = err.Error()
					return
				}
				defer st.Close()
				var out strings.Builder
				sawFirst := false
				for {
					ev, err := st.Next()
					if err == io.EOF {
						break
					}
					if err != nil {
						c.Err = err.Error()
						break
					}
					if !sawFirst && (ev.Kind == core.EventTextDelta || ev.Kind == core.EventThinkingDelta || ev.Kind == core.EventToolCallStart) {
						sawFirst = true
						c.TTFTMS = time.Since(start).Milliseconds()
					}
					if ev.Kind == core.EventTextDelta {
						out.WriteString(ev.Text)
					}
					if ev.Kind == core.EventUsage && ev.Usage != nil {
						c.TokensIn, c.TokensOut = ev.Usage.InputTokens, ev.Usage.OutputTokens
					}
				}
				c.TotalMS = time.Since(start).Milliseconds()
				c.Output = strings.TrimSpace(out.String())
				if in, outP, priced := reg.Price([]string{provName}, model); priced {
					cost := (float64(c.TokensIn)*in + float64(c.TokensOut)*outP) / 1e6
					c.CostUSD = &cost
				}
			}(si, spec)
		}
		wg.Wait()
	}

	for pi, prompt := range prompts {
		short := prompt
		if len(short) > 90 {
			short = short[:90] + "…"
		}
		fmt.Printf("\nPROMPT %d: %s\n", pi+1, short)
		fmt.Printf("%-42s %8s %9s %7s %8s %10s  %s\n", "MODEL", "TTFT_MS", "TOTAL_MS", "TOK_IN", "TOK_OUT", "COST_USD", "OUTPUT")
		for _, c := range grid[pi] {
			if c.Err != "" {
				fmt.Printf("%-42s %s\n", c.Spec, "ERROR: "+c.Err)
				continue
			}
			cost := "—" // unpriced stays honest, same as everywhere else
			if c.CostUSD != nil {
				cost = fmt.Sprintf("%.6f", *c.CostUSD)
			}
			snippet := strings.ReplaceAll(c.Output, "\n", " ")
			if len(snippet) > 60 {
				snippet = snippet[:60] + "…"
			}
			fmt.Printf("%-42s %8d %9d %7d %8d %10s  %s\n", c.Spec, c.TTFTMS, c.TotalMS, c.TokensIn, c.TokensOut, cost, snippet)
		}
	}

	if *htmlPath != "" {
		var b strings.Builder
		b.WriteString(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>relay compare</title><style>
:root { color-scheme: light dark; --line: #8884; --dim: #888; }
body { font: 14px/1.5 system-ui, sans-serif; margin: 2rem auto; max-width: 76rem; padding: 0 1rem; }
h1 { font-size: 1.3rem; } h2 { font-size: 1rem; margin-top: 2rem; color: var(--dim); font-weight: 600; }
.row { display: flex; gap: 1rem; flex-wrap: wrap; }
.card { flex: 1 1 20rem; border: 1px solid var(--line); border-radius: 8px; padding: .8rem 1rem; }
.card h3 { margin: 0 0 .4rem; font-size: .95rem; }
.stats { color: var(--dim); font-size: .85rem; margin-bottom: .6rem; }
pre { white-space: pre-wrap; font: .85rem/1.45 ui-monospace, monospace; margin: 0; }
.err { color: #c33; }
</style></head><body><h1>relay compare</h1>`)
		for pi, prompt := range prompts {
			fmt.Fprintf(&b, "<h2>Prompt %d — %s</h2><div class=\"row\">", pi+1, html.EscapeString(prompt))
			for _, c := range grid[pi] {
				fmt.Fprintf(&b, "<div class=\"card\"><h3>%s</h3>", html.EscapeString(c.Spec))
				if c.Err != "" {
					fmt.Fprintf(&b, "<pre class=\"err\">%s</pre></div>", html.EscapeString(c.Err))
					continue
				}
				cost := "— (unpriced)"
				if c.CostUSD != nil {
					cost = fmt.Sprintf("$%.6f", *c.CostUSD)
				}
				fmt.Fprintf(&b, "<div class=\"stats\">TTFT %d ms · total %d ms · %d→%d tokens · %s</div><pre>%s</pre></div>",
					c.TTFTMS, c.TotalMS, c.TokensIn, c.TokensOut, cost, html.EscapeString(c.Output))
			}
			b.WriteString("</div>")
		}
		b.WriteString("</body></html>")
		if err := os.WriteFile(*htmlPath, []byte(b.String()), 0o644); err != nil {
			return err
		}
		fmt.Printf("\nwrote %s\n", *htmlPath)
	}
	return nil
}
