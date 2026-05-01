// Package testutil provides shared test helpers for internal/node packages.
package testutil

import (
	"context"
	"strings"
	"sync"
)

// Response is a single canned output for a FakeRunner call.
type Response struct {
	Out []byte
	Err error
}

// FakeRunner is a stateful test double for the Runner interface used across
// internal/node packages.  Each key in Resps holds a slice of responses that
// are consumed in order: the first call to that command gets index 0, the
// second gets index 1, and so on.  Once the slice is exhausted every further
// call repeats the last entry.  If a full "name arg0 arg1..." key is not
// found, FakeRunner falls back to the bare "name" key.  An unmatched command
// returns nil, nil.
type FakeRunner struct {
	mu sync.Mutex

	// Resps maps "name arg0 arg1..." (or bare "name") to a response sequence.
	Resps map[string][]Response

	// Calls records the full command string for every invocation.
	Calls []string
}

// NewFakeRunner returns a FakeRunner with an initialised Resps map.
func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Resps: map[string][]Response{}}
}

// Run implements the Runner interface.  env is ignored; use RunEnv for that.
func (r *FakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := buildKey(name, args)

	r.mu.Lock()
	defer r.mu.Unlock()

	r.Calls = append(r.Calls, key)

	if resp, ok := r.consumeNext(key); ok {
		return resp.Out, resp.Err
	}
	// Fallback to bare name key.
	if resp, ok := r.consumeNext(name); ok {
		return resp.Out, resp.Err
	}
	return nil, nil
}

// RunEnv implements the RunnerEnv interface; env is ignored in tests and the
// call is delegated to Run.
func (r *FakeRunner) RunEnv(ctx context.Context, _ []string, name string, args ...string) ([]byte, error) {
	return r.Run(ctx, name, args...)
}

// consumeNext returns the next Response for key, advancing the cursor.  The
// last entry is kept so it repeats indefinitely once the slice is exhausted.
// Must be called with r.mu held.
func (r *FakeRunner) consumeNext(key string) (Response, bool) {
	q, ok := r.Resps[key]
	if !ok || len(q) == 0 {
		return Response{}, false
	}
	resp := q[0]
	if len(q) > 1 {
		r.Resps[key] = q[1:]
	}
	// If len(q) == 1, leave the single entry so it repeats.
	return resp, true
}

func buildKey(name string, args []string) string {
	if len(args) == 0 {
		return name
	}
	return name + " " + strings.Join(args, " ")
}
