// Package store is the local SQLite request log. Writes are asynchronous
// through a bounded queue: logging never blocks or fails a request; on
// overflow, records are dropped and counted.
package store

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; keeps CGO_ENABLED=0
)

// schema matches DESIGN §9. prompt_embedding and feedback_score are
// reserved now so phase-1 logs are usable as phase-4 training data.
const schema = `
CREATE TABLE IF NOT EXISTS requests (
  id               TEXT PRIMARY KEY,
  ts               INTEGER NOT NULL,
  api              TEXT NOT NULL,
  model_requested  TEXT NOT NULL,
  model_served     TEXT,
  provider         TEXT,
  route_policy     TEXT,
  route_reason     TEXT,
  attempts         INTEGER,
  candidates_json  TEXT,
  status           INTEGER,
  error_code       TEXT,
  tokens_in        INTEGER,
  tokens_out       INTEGER,
  cost_usd         REAL,
  latency_ms       INTEGER,
  ttft_ms          INTEGER,
  stream           INTEGER,
  cached           INTEGER,
  prompt_hash      TEXT,
  prompt_embedding BLOB,
  prompt_body      TEXT,
  response_body    TEXT,
  feedback_score   REAL
);
CREATE INDEX IF NOT EXISTS idx_requests_ts ON requests(ts);
CREATE INDEX IF NOT EXISTS idx_requests_provider ON requests(provider, ts);
`

// Record is one logged request.
type Record struct {
	ID             string
	TS             time.Time
	API            string
	ModelRequested string
	ModelServed    string
	Provider       string
	RoutePolicy    string
	RouteReason    string
	Attempts       int
	Status         int
	ErrorCode      string
	TokensIn       int
	TokensOut      int
	CostUSD        *float64 // nil = model not in the pricing registry (logged as NULL)
	LatencyMS      int64
	TTFTMS         int64
	Stream         bool
	Cached         bool
	PromptHash     string
	// PromptEmbedding is the query vector under log_prompts: embeddings —
	// the training-value tier that never stores raw text (DESIGN §8).
	PromptEmbedding []float32
	PromptBody      string
	ResponseBody    string
}

// Store owns the database and the async writer.
type Store struct {
	db      *sql.DB
	ch      chan Record
	done    chan struct{} // closed by Close; ch itself is never closed
	wg      sync.WaitGroup
	dropped atomic.Int64
	closed  atomic.Bool
}

// Open creates or opens the database at path, applying the schema.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating db directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	// modernc/sqlite serializes writes; one connection avoids lock churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	s := &Store{db: db, ch: make(chan Record, 1024), done: make(chan struct{})}
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

func (s *Store) writer() {
	defer s.wg.Done()
	write := func(r Record) {
		if err := s.insert(r); err != nil {
			// Logging must never take down serving; count and continue.
			s.dropped.Add(1)
		}
	}
	for {
		select {
		case r := <-s.ch:
			write(r)
		case <-s.done:
			// Drain what is buffered, then exit. A Log that raced past the
			// closed check during shutdown may miss this drain; that record
			// is best-effort by design.
			for {
				select {
				case r := <-s.ch:
					write(r)
				default:
					return
				}
			}
		}
	}
}

func (s *Store) insert(r Record) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO requests
		(id, ts, api, model_requested, model_served, provider, route_policy,
		 route_reason, attempts, status, error_code, tokens_in, tokens_out,
		 cost_usd, latency_ms, ttft_ms, stream, cached, prompt_hash,
		 prompt_embedding, prompt_body, response_body)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.TS.UnixMilli(), r.API, r.ModelRequested, r.ModelServed,
		r.Provider, r.RoutePolicy, r.RouteReason, r.Attempts, r.Status,
		r.ErrorCode, r.TokensIn, r.TokensOut, r.CostUSD, r.LatencyMS,
		r.TTFTMS, boolInt(r.Stream), boolInt(r.Cached), r.PromptHash,
		EncodeVector(r.PromptEmbedding), nullable(r.PromptBody), nullable(r.ResponseBody))
	return err
}

// EncodeVector packs a float32 vector as little-endian bytes for the
// prompt_embedding BLOB (DESIGN §9); nil in, nil (SQL NULL) out.
func EncodeVector(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[4*i:], math.Float32bits(f))
	}
	return out
}

// DecodeVector unpacks a prompt_embedding BLOB.
func DecodeVector(b []byte) []float32 {
	if len(b) < 4 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// Log enqueues a record without blocking; on a full queue it is dropped
// and counted.
func (s *Store) Log(r Record) {
	if s.closed.Load() {
		return
	}
	select {
	case s.ch <- r:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many records were lost to backpressure or errors.
func (s *Store) Dropped() int64 { return s.dropped.Load() }

// Close drains the queue and closes the database. The record channel is
// never closed: a request goroutine that raced past Log's closed check
// must be able to complete its send without panicking (-race gate finding,
// 2026-07-18).
func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.done)
	s.wg.Wait()
	return s.db.Close()
}

// SetFeedback records an explicit quality score for a logged request
// (POST /v1/feedback, DESIGN §0.4). Returns false when no such request
// id exists. Synchronous: feedback is rare and callers want the 404.
func (s *Store) SetFeedback(requestID string, score float64) (bool, error) {
	if s.closed.Load() {
		return false, fmt.Errorf("store is closed")
	}
	res, err := s.db.Exec(`UPDATE requests SET feedback_score = ? WHERE id = ?`, score, requestID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DB exposes the underlying handle for read-only stats queries.
func (s *Store) DB() *sql.DB { return s.db }
