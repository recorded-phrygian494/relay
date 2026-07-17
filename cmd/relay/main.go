// Command relay is a self-hosted LLM gateway and model router.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/llmrelay/relay/internal/config"
	"github.com/llmrelay/relay/internal/pricing"
	"github.com/llmrelay/relay/internal/server"
	"github.com/llmrelay/relay/internal/store"
)

var version = "0.1.0-dev" // overridden by goreleaser ldflags in phase 5

const usage = `relay — self-hosted LLM gateway and model router

Usage:
  relay [serve]           run the gateway (default)
  relay init              scaffold a relay.yaml
  relay stats             traffic summary from the local request log
  relay pricing update    refresh the pricing registry (explicit only, never automatic)
  relay pricing show      print the active pricing registry source
  relay version           print the version

Flags for serve:
  --config PATH        config file (default: ./relay.yaml, ~/.relay/relay.yaml, or zero-config)
  --listen ADDR        override the listen address
  --db PATH            override the request-log path
  --insecure-no-auth   allow a non-loopback listener without inbound API keys
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && !isFlag(args[0]) {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = runServe(args)
	case "init":
		err = runInit(args)
	case "stats":
		err = runStats(args)
	case "pricing":
		err = runPricing(args)
	case "version":
		fmt.Println("relay", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		log.Fatalf("relay %s: %v", cmd, err)
	}
}

func isFlag(s string) bool { return len(s) > 0 && s[0] == '-' }

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path")
	listen := fs.String("listen", "", "listen address override")
	dbPath := fs.String("db", "", "request-log path override")
	insecure := fs.Bool("insecure-no-auth", false, "allow non-loopback listen without inbound API keys")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var cfg *config.Config
	path := config.Find(*cfgPath)
	if path == "" {
		cfg = config.Sniff()
		log.Printf("no relay.yaml found; zero-config mode with %d provider(s)", len(cfg.Providers))
	} else {
		var err error
		if cfg, err = config.Load(path); err != nil {
			return err
		}
		log.Printf("loaded config from %s", path)
	}
	for _, w := range cfg.Warnings {
		log.Printf("config warning: %s", w)
	}
	if *listen != "" {
		cfg.Server.Listen = *listen
	}
	if *dbPath != "" {
		cfg.Logging.DB = *dbPath
	}

	// DESIGN §0.6: an open proxy that spends your provider keys must not
	// ship. Refuse a non-loopback bind without inbound auth.
	if !server.IsLoopback(cfg.Server.Listen) && len(cfg.Server.APIKeys) == 0 && !*insecure {
		return fmt.Errorf("refusing to listen on non-loopback %q without server.api_keys; set a key or pass --insecure-no-auth", cfg.Server.Listen)
	}

	st, err := store.Open(cfg.Logging.DB)
	if err != nil {
		return fmt.Errorf("opening request log: %w", err)
	}
	defer st.Close()

	rt, err := server.BuildRuntime(cfg)
	if err != nil {
		return err
	}
	srv := server.New(rt, st, version)

	var stopWatch func()
	if path != "" {
		stopWatch = config.Watch(path, 2*time.Second,
			func(newCfg *config.Config) {
				if *listen != "" {
					newCfg.Server.Listen = *listen
				}
				newRT, err := server.BuildRuntime(newCfg)
				if err != nil {
					log.Printf("config reload failed (keeping previous config): %v", err)
					return
				}
				srv.Swap(newRT)
				log.Printf("config reloaded from %s", path)
			},
			func(err error) { log.Printf("config reload failed (keeping previous config): %v", err) },
		)
		defer stopWatch()
	}

	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
		defer stop()
		<-ctx.Done()
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()

	for name := range rt.Providers {
		log.Printf("provider ready: %s", name)
	}
	log.Printf("relay %s listening on http://%s (OpenAI-compatible: /v1/chat/completions)", version, cfg.Server.Listen)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

const initTemplate = `# relay.yaml — see DESIGN.md for the full schema
server:
  listen: "127.0.0.1:4000"
  # api_keys: ["${RELAY_API_KEY}"]   # required for non-loopback listeners

providers:
  openai:
    api_key: ["${OPENAI_API_KEY}"]
  # groq:
  #   profile: groq                  # preset base_url; see openaicompat profiles
  #   api_key: ["${GROQ_API_KEY}"]
  # local:
  #   type: openai-compat            # vLLM, LM Studio, llama.cpp server, ...
  #   base_url: "http://localhost:8000/v1"
  ollama:
    type: ollama                     # auto-discovers local models

routing:
  # static: { fast: "ollama/llama3.2:latest" }
  # default_provider: openai

logging:
  # db: "~/.relay/relay.db"
  log_prompts: off                   # off | embeddings | full
`

func runInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	force := fs.Bool("force", false, "overwrite an existing relay.yaml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat("relay.yaml"); err == nil && !*force {
		return fmt.Errorf("relay.yaml already exists (use --force to overwrite)")
	}
	if err := os.WriteFile("relay.yaml", []byte(initTemplate), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote relay.yaml — set your API keys as environment variables and run: relay serve")
	return nil
}

// pricingURL is the project's published registry; fetched only on explicit
// `relay pricing update` (DESIGN §9 — automatic refresh would be phone-home).
const pricingURL = "https://raw.githubusercontent.com/llmrelay/relay/main/assets/pricing.json"

func runPricing(args []string) error {
	sub := ""
	if len(args) > 0 {
		sub, args = args[0], args[1:]
	}
	switch sub {
	case "show":
		reg, err := pricing.Load()
		if err != nil {
			log.Printf("%v", err)
		}
		src := "embedded snapshot"
		if _, statErr := os.Stat(pricing.OverridePath()); statErr == nil && err == nil {
			src = pricing.OverridePath()
		}
		fmt.Printf("pricing registry version %s (%s)\n", reg.Version, src)
		return nil
	case "update":
		fs := flag.NewFlagSet("pricing update", flag.ExitOnError)
		url := fs.String("url", pricingURL, "registry URL to fetch")
		if err := fs.Parse(args); err != nil {
			return err
		}
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(*url)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("fetching %s: status %d", *url, resp.StatusCode)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return err
		}
		if err := pricing.Validate(raw); err != nil {
			return fmt.Errorf("fetched registry is invalid, not saving: %w", err)
		}
		dest := pricing.OverridePath()
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", dest)
		return nil
	default:
		return fmt.Errorf("usage: relay pricing <update|show>")
	}
}

func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dbPath := fs.String("db", "", "request-log path")
	since := fs.Duration("since", 24*time.Hour, "window to summarize")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := *dbPath
	if path == "" {
		path = config.DefaultDBPath()
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no request log at %s (is relay running with defaults, or pass --db)", path)
	}
	st, err := store.Open(path)
	if err != nil {
		return err
	}
	defer st.Close()
	stats, err := store.Stats(st.DB(), time.Now().Add(-*since))
	if err != nil {
		return err
	}
	fmt.Printf("relay stats — last %s\n\n%s", *since, store.FormatTable(stats))
	return nil
}
