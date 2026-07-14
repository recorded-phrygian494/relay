package openai

import (
	"encoding/json"
	"testing"
)

// FuzzParseChatRequest enforces the zero-panic bar on the inbound parser
// (DESIGN quality bars). The seed corpus doubles as a unit test in CI.
func FuzzParseChatRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"c","function":{"name":"f","arguments":"{}"}}]}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":"x","n":2}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`{"model":123}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"bogus","content":"hi"}]}`))
	f.Add([]byte(`{"model":"m","messages":[{"content":{"deep":{"nesting":[1,2,{"x":null}]}}}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := ParseChatRequest(body) // must never panic
		if err != nil || req == nil {
			return
		}
		// Anything that parsed must survive re-marshaling (round-trip safety).
		if _, err := json.Marshal(req); err != nil {
			t.Fatalf("parsed request failed to marshal: %v", err)
		}
	})
}
