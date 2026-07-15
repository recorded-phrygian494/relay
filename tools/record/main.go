// Command record captures ground-truth fixtures from real provider APIs:
// the exact request body sent and the exact response body (or raw SSE/NDJSON
// stream bytes) received, with no relay code in the path. Maintainers run it
// locally; CI never does. Recorded reality beats documentation (DESIGN §5.3).
//
//	go run ./tools/record --dialect anthropic --key-env ANTHROPIC_API_KEY --model claude-haiku-4-5-20251001
//	go run ./tools/record --dialect openai --base-url http://localhost:11434/v1 --model mistral:latest
//	go run ./tools/record --dialect ollama --model mistral-nemo:latest
//	go run ./tools/record --dialect gemini --key-env GEMINI_API_KEY --model gemini-2.0-flash
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type scenario struct {
	name   string
	body   string // {{MODEL}} is substituted
	stream bool
}

// A tiny valid 1x1 red PNG for vision scenarios.
const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

var openaiScenarios = []scenario{
	{name: "simple", body: `{"model":"{{MODEL}}","messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"Say exactly: hello relay"}],"max_tokens":40,"temperature":0}`},
	{name: "tools", body: `{"model":"{{MODEL}}","messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"required","max_tokens":120,"temperature":0}`},
	{name: "stream_text", stream: true, body: `{"model":"{{MODEL}}","messages":[{"role":"user","content":"Count from 1 to 5, digits only."}],"max_tokens":60,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}`},
	{name: "stream_tools", stream: true, body: `{"model":"{{MODEL}}","messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"tool_choice":"required","max_tokens":120,"temperature":0,"stream":true,"stream_options":{"include_usage":true}}`},
}

var anthropicScenarios = []scenario{
	{name: "simple", body: `{"model":"{{MODEL}}","max_tokens":40,"system":"You are terse.","messages":[{"role":"user","content":"Say exactly: hello relay"}],"temperature":0}`},
	{name: "tools", body: `{"model":"{{MODEL}}","max_tokens":200,"messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"any"},"temperature":0}`},
	{name: "tool_result", body: `{"model":"{{MODEL}}","max_tokens":100,"messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_record1","name":"get_weather","input":{"city":"Dublin"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_record1","content":"14C, raining"}]}],"tools":[{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"temperature":0}`},
	{name: "vision", body: `{"model":"{{MODEL}}","max_tokens":60,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png1x1 + `"}},{"type":"text","text":"One word: what color is this pixel?"}]}],"temperature":0}`},
	{name: "stream_text", stream: true, body: `{"model":"{{MODEL}}","max_tokens":60,"messages":[{"role":"user","content":"Count from 1 to 5, digits only."}],"temperature":0,"stream":true}`},
	{name: "stream_tools", stream: true, body: `{"model":"{{MODEL}}","max_tokens":200,"messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"name":"get_weather","description":"Get current weather for a city","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],"tool_choice":{"type":"any"},"temperature":0,"stream":true}`},
	{name: "forced_schema_tool", body: `{"model":"{{MODEL}}","max_tokens":200,"messages":[{"role":"user","content":"Extract the name and age: John is 30"}],"tools":[{"name":"emit_structured_output","description":"Emit the answer as structured output matching the schema.","input_schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}}],"tool_choice":{"type":"tool","name":"emit_structured_output"},"temperature":0}`},
	{name: "forced_schema_tool_stream", stream: true, body: `{"model":"{{MODEL}}","max_tokens":200,"messages":[{"role":"user","content":"Extract the name and age: John is 30"}],"tools":[{"name":"emit_structured_output","description":"Emit the answer as structured output matching the schema.","input_schema":{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"}},"required":["name","age"]}}],"tool_choice":{"type":"tool","name":"emit_structured_output"},"temperature":0,"stream":true}`},
}

var geminiScenarios = []scenario{
	{name: "simple", body: `{"systemInstruction":{"parts":[{"text":"You are terse."}]},"contents":[{"role":"user","parts":[{"text":"Say exactly: hello relay"}]}],"generationConfig":{"maxOutputTokens":40,"temperature":0}}`},
	{name: "tools", body: `{"contents":[{"role":"user","parts":[{"text":"What is the weather in Dublin? Use the tool."}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"maxOutputTokens":120,"temperature":0}}`},
	{name: "stream_text", stream: true, body: `{"contents":[{"role":"user","parts":[{"text":"Count from 1 to 5, digits only."}]}],"generationConfig":{"maxOutputTokens":60,"temperature":0}}`},
	{name: "stream_tools", stream: true, body: `{"contents":[{"role":"user","parts":[{"text":"What is the weather in Dublin? Use the tool."}]}],"tools":[{"functionDeclarations":[{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY"}},"generationConfig":{"maxOutputTokens":120,"temperature":0}}`},
}

var ollamaScenarios = []scenario{
	{name: "simple", body: `{"model":"{{MODEL}}","stream":false,"messages":[{"role":"system","content":"You are terse."},{"role":"user","content":"Say exactly: hello relay"}],"options":{"temperature":0,"num_predict":40}}`},
	{name: "tools", body: `{"model":"{{MODEL}}","stream":false,"messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"options":{"temperature":0,"num_predict":200}}`},
	{name: "stream_text", stream: true, body: `{"model":"{{MODEL}}","stream":true,"messages":[{"role":"user","content":"Count from 1 to 5, digits only."}],"options":{"temperature":0,"num_predict":60}}`},
	{name: "stream_tools", stream: true, body: `{"model":"{{MODEL}}","stream":true,"messages":[{"role":"user","content":"What is the weather in Dublin? Use the tool."}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],"options":{"temperature":0,"num_predict":200}}`},
}

type dialectSpec struct {
	scenarios      []scenario
	defaultBaseURL string
	path           func(model string, stream bool) string
	headers        func(key string) map[string]string
}

var dialects = map[string]dialectSpec{
	"openai": {
		scenarios:      openaiScenarios,
		defaultBaseURL: "https://api.openai.com/v1",
		path:           func(string, bool) string { return "/chat/completions" },
		headers: func(key string) map[string]string {
			return map[string]string{"Authorization": "Bearer " + key}
		},
	},
	"anthropic": {
		scenarios:      anthropicScenarios,
		defaultBaseURL: "https://api.anthropic.com",
		path:           func(string, bool) string { return "/v1/messages" },
		headers: func(key string) map[string]string {
			return map[string]string{"x-api-key": key, "anthropic-version": "2023-06-01"}
		},
	},
	"gemini": {
		scenarios:      geminiScenarios,
		defaultBaseURL: "https://generativelanguage.googleapis.com/v1beta",
		path: func(model string, stream bool) string {
			if stream {
				return "/models/" + model + ":streamGenerateContent?alt=sse"
			}
			return "/models/" + model + ":generateContent"
		},
		headers: func(key string) map[string]string {
			return map[string]string{"x-goog-api-key": key}
		},
	},
	"ollama": {
		scenarios:      ollamaScenarios,
		defaultBaseURL: "http://localhost:11434",
		path:           func(string, bool) string { return "/api/chat" },
		headers:        func(string) map[string]string { return nil },
	},
}

func main() {
	dialect := flag.String("dialect", "", "openai | anthropic | gemini | ollama")
	baseURL := flag.String("base-url", "", "override the API base URL")
	keyEnv := flag.String("key-env", "", "environment variable holding the API key")
	model := flag.String("model", "", "model id to record against")
	out := flag.String("out", "", "output dir (default internal/translate/testdata/<dialect>/recorded)")
	only := flag.String("scenario", "", "record only this scenario")
	flag.Parse()

	spec, ok := dialects[*dialect]
	if !ok || *model == "" {
		fmt.Fprintln(os.Stderr, "usage: record --dialect <openai|anthropic|gemini|ollama> --model <id> [--base-url URL] [--key-env VAR] [--scenario NAME] [--out DIR]")
		os.Exit(2)
	}
	url := spec.defaultBaseURL
	if *baseURL != "" {
		url = strings.TrimRight(*baseURL, "/")
	}
	key := ""
	if *keyEnv != "" {
		key = os.Getenv(*keyEnv)
		if key == "" {
			fmt.Fprintf(os.Stderr, "record: %s is empty\n", *keyEnv)
			os.Exit(1)
		}
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join("internal", "translate", "testdata", *dialect, "recorded")
	}

	client := &http.Client{Timeout: 5 * time.Minute}
	failures := 0
	for _, sc := range spec.scenarios {
		if *only != "" && sc.name != *only {
			continue
		}
		if err := record(client, spec, url, key, *model, outDir, sc); err != nil {
			fmt.Fprintf(os.Stderr, "  FAIL %s: %v\n", sc.name, err)
			failures++
		}
	}
	if failures > 0 {
		os.Exit(1)
	}
}

func record(client *http.Client, spec dialectSpec, baseURL, key, model, outDir string, sc scenario) error {
	body := strings.ReplaceAll(sc.body, "{{MODEL}}", model)
	dir := filepath.Join(outDir, sc.name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+spec.path(model, sc.stream), bytes.NewReader([]byte(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		for k, v := range spec.headers(key) {
			req.Header.Set(k, v)
		}
	}
	if sc.stream {
		req.Header.Set("Accept", "text/event-stream")
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}

	if err := os.WriteFile(filepath.Join(dir, "request.json"), []byte(body), 0o644); err != nil {
		return err
	}
	respName := "response.json"
	if sc.stream {
		respName = "stream.txt"
	}
	if err := os.WriteFile(filepath.Join(dir, respName), raw, 0o644); err != nil {
		return err
	}
	fmt.Printf("  ok %-26s %6dB in %s\n", sc.name, len(raw), time.Since(start).Round(time.Millisecond))
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
