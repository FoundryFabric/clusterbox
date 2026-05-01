package testutil_test

import (
	"context"
	"errors"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/node/testutil"
)

// TestSingleResponse verifies that a single-entry sequence is returned for
// every call (last-entry-repeats behaviour with a one-element slice).
func TestSingleResponse(t *testing.T) {
	r := testutil.NewFakeRunner()
	want := []byte("hello")
	r.Resps["echo"] = []testutil.Response{{Out: want}}

	for i := 0; i < 3; i++ {
		out, err := r.Run(context.Background(), "echo")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", i, err)
		}
		if string(out) != string(want) {
			t.Fatalf("call %d: got %q, want %q", i, out, want)
		}
	}
}

// TestSequence verifies that successive calls consume responses in order and
// that the last entry repeats once the sequence is exhausted.
func TestSequence(t *testing.T) {
	r := testutil.NewFakeRunner()
	r.Resps["tailscale status --json"] = []testutil.Response{
		{Out: []byte("starting")},
		{Out: []byte("running")},
	}

	check := func(call int, wantOut string) {
		t.Helper()
		out, err := r.Run(context.Background(), "tailscale", "status", "--json")
		if err != nil {
			t.Fatalf("call %d: unexpected error: %v", call, err)
		}
		if string(out) != wantOut {
			t.Fatalf("call %d: got %q, want %q", call, out, wantOut)
		}
	}

	check(1, "starting") // consumes index 0
	check(2, "running")  // consumes index 1
	check(3, "running")  // last entry repeats
	check(4, "running")  // still repeating
}

// TestFallbackToNameKey verifies that when the full "name arg0 arg1..." key is
// not present, FakeRunner falls back to the bare command name.
func TestFallbackToNameKey(t *testing.T) {
	r := testutil.NewFakeRunner()
	r.Resps["systemctl"] = []testutil.Response{{Out: []byte("active")}}

	out, err := r.Run(context.Background(), "systemctl", "is-active", "k3s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "active" {
		t.Fatalf("got %q, want %q", out, "active")
	}
}

// TestFullKeyTakesPrecedenceOverNameKey ensures that the specific full-command
// key wins over the bare name fallback.
func TestFullKeyTakesPrecedenceOverNameKey(t *testing.T) {
	r := testutil.NewFakeRunner()
	r.Resps["systemctl is-active k3s"] = []testutil.Response{{Out: []byte("active")}}
	r.Resps["systemctl"] = []testutil.Response{{Out: []byte("inactive")}}

	out, err := r.Run(context.Background(), "systemctl", "is-active", "k3s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(out) != "active" {
		t.Fatalf("got %q, want %q", out, "active")
	}
}

// TestUnmappedCommandReturnsNilNil confirms that an unmapped command returns
// nil output and nil error.
func TestUnmappedCommandReturnsNilNil(t *testing.T) {
	r := testutil.NewFakeRunner()

	out, err := r.Run(context.Background(), "unknown", "arg")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil output, got %q", out)
	}
}

// TestCallsLog verifies that every invocation is recorded in Calls.
func TestCallsLog(t *testing.T) {
	r := testutil.NewFakeRunner()

	r.Run(context.Background(), "foo", "bar") //nolint:errcheck
	r.Run(context.Background(), "baz")        //nolint:errcheck
	r.Run(context.Background(), "foo", "bar") //nolint:errcheck

	want := []string{"foo bar", "baz", "foo bar"}
	if len(r.Calls) != len(want) {
		t.Fatalf("len(Calls) = %d, want %d; calls = %v", len(r.Calls), len(want), r.Calls)
	}
	for i, w := range want {
		if r.Calls[i] != w {
			t.Errorf("Calls[%d] = %q, want %q", i, r.Calls[i], w)
		}
	}
}

// TestRunEnvDelegatesToRun verifies that RunEnv records the call and returns
// the response exactly as Run would (env is ignored).
func TestRunEnvDelegatesToRun(t *testing.T) {
	r := testutil.NewFakeRunner()
	wantErr := errors.New("oops")
	r.Resps["service restart"] = []testutil.Response{{Err: wantErr}}

	_, err := r.RunEnv(context.Background(), []string{"FOO=bar"}, "service", "restart")
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}

	if len(r.Calls) != 1 || r.Calls[0] != "service restart" {
		t.Fatalf("unexpected Calls: %v", r.Calls)
	}
}
