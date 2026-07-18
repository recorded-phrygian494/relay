// Package cache implements the exact-match response cache (DESIGN §10):
// opt-in, TTL-bounded, memory LRU. Keys are the SHA-256 of the canonical
// IR after inbound translation, so dialect variants of the same request
// share an entry. Only successful responses are ever stored — error
// states are non-deterministic and must not be replayed. The Cache
// interface is the semantic-cache extension point; Exact is v1's only
// implementation.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/llmrelay/relay/internal/core"
)

// Entry is one cached completion, with the provider/model that produced
// it (for the request log on replay).
type Entry struct {
	Response *core.Response
	Provider string
	Model    string
}

// Cache is the extension point: exact today, semantic later.
type Cache interface {
	Get(key string) (*Entry, bool)
	Put(key string, e *Entry)
}

// Cacheable reports whether a request may be served from / stored into
// the cache: deterministic requests only — temperature pinned to 0 — or
// an explicit client opt-in via the x-relay-cache: allow header.
func Cacheable(ir *core.Request, headerValue string) bool {
	if headerValue == "allow" {
		return true
	}
	return ir.Temperature != nil && *ir.Temperature == 0
}

// Key canonicalizes the IR and hashes it. Stream flags and the inbound
// dialect are erased first: a streamed OpenAI request and a non-streamed
// Anthropic request with the same content hit the same entry.
func Key(ir *core.Request) string {
	c := *ir
	c.Stream = false
	c.IncludeStreamUsage = false
	c.Inbound = ""
	b, err := json.Marshal(&c)
	if err != nil {
		// Marshal of the IR cannot fail (no cycles, no unsupported types);
		// treat a failure as uncacheable rather than colliding.
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Exact is a TTL'd LRU of exact-match entries. Safe for concurrent use.
type Exact struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	ll    *list.List // front = most recent
	items map[string]*list.Element
	now   func() time.Time
}

type item struct {
	key     string
	entry   *Entry
	expires time.Time
}

// NewExact builds the cache. maxEntries <= 0 selects the default (1024).
func NewExact(ttl time.Duration, maxEntries int) *Exact {
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	return &Exact{
		ttl:   ttl,
		max:   maxEntries,
		ll:    list.New(),
		items: make(map[string]*list.Element),
		now:   time.Now,
	}
}

// Get implements Cache. Expired entries are evicted on access.
func (c *Exact) Get(key string) (*Entry, bool) {
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	it := el.Value.(*item)
	if c.now().After(it.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return it.entry, true
}

// Put implements Cache, evicting the least-recently-used entry past the
// size cap.
func (c *Exact) Put(key string, e *Entry) {
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		it := el.Value.(*item)
		it.entry, it.expires = e, c.now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&item{key: key, entry: e, expires: c.now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		oldest := c.ll.Back()
		c.ll.Remove(oldest)
		delete(c.items, oldest.Value.(*item).key)
	}
}

// Events synthesizes the streaming replay of a cached response: the same
// canonical events a live upstream would have produced (DESIGN §10
// "streams replay as synthetic events").
func Events(resp *core.Response) []core.Event {
	events := []core.Event{{Kind: core.EventMessageStart, ID: resp.ID, Model: resp.Model}}
	for _, choice := range resp.Choices {
		toolIdx := 0
		for _, part := range choice.Parts {
			switch p := part.(type) {
			case core.TextPart:
				events = append(events, core.Event{Kind: core.EventTextDelta, Choice: choice.Index, Text: p.Text})
			case core.ThinkingPart:
				switch {
				case p.Redacted != "":
					events = append(events, core.Event{Kind: core.EventRedactedThinking, Choice: choice.Index, Text: p.Redacted})
				default:
					events = append(events, core.Event{Kind: core.EventThinkingDelta, Choice: choice.Index, Text: p.Text})
					if p.Signature != "" {
						events = append(events, core.Event{Kind: core.EventSignatureDelta, Choice: choice.Index, Text: p.Signature})
					}
				}
			case core.ToolCallPart:
				events = append(events,
					core.Event{Kind: core.EventToolCallStart, Choice: choice.Index, ToolIndex: toolIdx, ToolID: p.ID, ToolName: p.Name},
					core.Event{Kind: core.EventToolCallDelta, Choice: choice.Index, ToolIndex: toolIdx, ArgsFragment: p.Args},
					core.Event{Kind: core.EventToolCallEnd, Choice: choice.Index, ToolIndex: toolIdx},
				)
				toolIdx++
			}
		}
	}
	usage := resp.Usage
	events = append(events, core.Event{Kind: core.EventUsage, Usage: &usage})
	for _, choice := range resp.Choices {
		events = append(events, core.Event{Kind: core.EventMessageEnd, Choice: choice.Index, StopReason: choice.StopReason})
	}
	return events
}
