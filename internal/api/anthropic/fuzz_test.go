package anthropic

import (
	"encoding/json"
	"testing"
)

// FuzzParseMessagesRequest enforces the zero-panic bar on the inbound
// /v1/messages parser.
func FuzzParseMessagesRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":10,"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"s","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"f","input":{"x":1}}]}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"ok"}]}]}]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"system","content":"nope"}]}`))
	f.Add([]byte(`{"model":"m","max_tokens":"ten","messages":[]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := ParseMessagesRequest(body, true) // must never panic
		if err != nil || req == nil {
			return
		}
		if _, err := json.Marshal(req); err != nil {
			t.Fatalf("parsed request failed to marshal: %v", err)
		}
	})
}
