package server

import (
	"net/http"
	"time"

	"github.com/llmrelay/relay/internal/cache"
	"github.com/llmrelay/relay/internal/core"
	"github.com/llmrelay/relay/internal/store"
)

// cacheLookup consults the exact-match cache (DESIGN §10). It returns the
// key ("" when the cache is off or the request is not cacheable) and, on a
// hit, the entry — the caller renders it in its own dialect. Hit/miss
// metrics and the request record are maintained here.
func (s *Server) cacheLookup(rt *Runtime, r *http.Request, ir *core.Request, rec *store.Record) (string, *cache.Entry) {
	if rt.Cache == nil || !cache.Cacheable(ir, r.Header.Get("x-relay-cache")) {
		return "", nil
	}
	key := cache.Key(ir)
	if key == "" {
		return "", nil
	}
	ent, ok := rt.Cache.Get(key)
	if !ok {
		s.metrics.CacheMisses.Inc()
		return key, nil
	}
	s.metrics.CacheHits.Inc()
	rec.Cached = true
	rec.Status = http.StatusOK
	rec.Provider, rec.ModelServed = ent.Provider, ent.Model
	rec.RoutePolicy, rec.RouteReason = "cache", "exact-match cache hit"
	return key, ent
}

// cacheStore stores a successful non-streaming response. Streaming
// responses are not re-assembled for storage in v1: streams can hit
// (synthetic replay) but only non-streamed completions populate.
func (rt *Runtime) cacheStore(key string, resp *core.Response, rec *store.Record) {
	if key == "" || rt.Cache == nil {
		return
	}
	rt.Cache.Put(key, &cache.Entry{Response: resp, Provider: rec.Provider, Model: rec.ModelServed})
}

// replayEvents streams a cached response as synthetic events through the
// dialect writer.
func replayEvents(resp *core.Response, w eventWriter, rec *store.Record) {
	// The replayed body mirrors the original completion, usage included;
	// the log keeps billed tokens at 0 — nothing was spent upstream.
	rec.TTFTMS = time.Since(rec.TS).Milliseconds()
	for _, ev := range cache.Events(resp) {
		if err := w.OnEvent(ev); err != nil {
			rec.ErrorCode = "client_disconnected"
			return
		}
	}
	_ = w.Done()
}
