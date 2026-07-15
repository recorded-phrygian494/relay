package router

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/llmrelay/relay/internal/core"
)

func testStatic() *Static {
	return &Static{
		Routes: map[string]string{"fast": "ollama/llama3.2:latest"},
		HasProvider: func(name string) bool {
			return name == "openai" || name == "ollama"
		},
		Catalog: func(context.Context) map[string][]string {
			return map[string][]string{
				"gpt-4o-mini":     {"openai"},
				"llama3.2:latest": {"ollama"},
				"shared-model":    {"openai", "ollama"},
			}
		},
	}
}

func route(t *testing.T, s *Static, model string) []Candidate {
	t.Helper()
	c, err := s.Route(context.Background(), &core.Request{Model: model})
	if err != nil {
		t.Fatalf("Route(%q): %v", model, err)
	}
	return c
}

func TestStaticExplicitPrefix(t *testing.T) {
	c := route(t, testStatic(), "openai/gpt-4o")
	if c[0].Provider != "openai" || c[0].Model != "gpt-4o" {
		t.Fatalf("got %+v", c[0])
	}
}

func TestStaticConfiguredRoute(t *testing.T) {
	c := route(t, testStatic(), "fast")
	if c[0].Provider != "ollama" || c[0].Model != "llama3.2:latest" {
		t.Fatalf("got %+v", c[0])
	}
}

func TestStaticUniqueCatalogModel(t *testing.T) {
	c := route(t, testStatic(), "gpt-4o-mini")
	if c[0].Provider != "openai" || c[0].Model != "gpt-4o-mini" {
		t.Fatalf("got %+v", c[0])
	}
}

func TestStaticAmbiguousModel(t *testing.T) {
	_, err := testStatic().Route(context.Background(), &core.Request{Model: "shared-model"})
	if err == nil || !strings.Contains(err.Error(), "2 providers") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func TestStaticDefaultProvider(t *testing.T) {
	s := testStatic()
	s.DefaultProvider = "openai"
	c := route(t, s, "some-future-model")
	if c[0].Provider != "openai" || c[0].Model != "some-future-model" {
		t.Fatalf("got %+v", c[0])
	}
}

func TestStaticNoRoute(t *testing.T) {
	_, err := testStatic().Route(context.Background(), &core.Request{Model: "unknown"})
	if !errors.Is(err, ErrNoRoute) {
		t.Fatalf("want ErrNoRoute, got %v", err)
	}
}

// A slash in the model name must not shadow a real model when the prefix is
// not a registered provider (e.g. Together serves "meta-llama/Llama-3-70b").
func TestStaticSlashModelNotAProvider(t *testing.T) {
	s := testStatic()
	s.Catalog = func(context.Context) map[string][]string {
		return map[string][]string{"meta-llama/Llama-3-70b": {"ollama"}}
	}
	c := route(t, s, "meta-llama/Llama-3-70b")
	if c[0].Provider != "ollama" || c[0].Model != "meta-llama/Llama-3-70b" {
		t.Fatalf("got %+v", c[0])
	}
}
