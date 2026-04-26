package provisiontest_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/foundryfabric/clusterbox/internal/provision/baremetal"
	"github.com/foundryfabric/clusterbox/internal/provision/baremetal/provisiontest"
)

// Compile-time guarantee MockTransport implements the production interface.
var _ baremetal.Transport = (*provisiontest.MockTransport)(nil)

func TestMockTransport_RunMatchesExactCommand(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"echo hello": {Stdout: []byte("hello\n"), ExitCode: 0},
			"false":      {ExitCode: 1},
		},
	}

	ctx := context.Background()
	stdout, _, exit, err := mt.Run(ctx, "echo hello", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if string(stdout) != "hello\n" || exit != 0 {
		t.Fatalf("got stdout=%q exit=%d", stdout, exit)
	}

	_, _, exit, err = mt.Run(ctx, "false", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if exit != 1 {
		t.Fatalf("expected exit=1, got %d", exit)
	}
}

func TestMockTransport_RunUnknownCommandFailsLoudly(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"echo hi": {Stdout: []byte("hi"), ExitCode: 0},
		},
	}

	_, _, exit, err := mt.Run(context.Background(), "echo bye", nil)
	if err == nil {
		t.Fatal("expected error for unmapped command, got nil")
	}
	if exit != -1 {
		t.Fatalf("expected exit=-1 on transport-err, got %d", exit)
	}
	calls := mt.RunCalls()
	if len(calls) != 1 || calls[0].Cmd != "echo bye" {
		t.Fatalf("expected one run call for unmapped cmd, got %+v", calls)
	}
}

func TestMockTransport_CapturesEnvOverlay(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"id": {ExitCode: 0},
		},
	}
	env := map[string]string{"FOO": "bar", "BAZ": "qux"}
	_, _, _, err := mt.Run(context.Background(), "id", env)
	if err != nil {
		t.Fatal(err)
	}
	calls := mt.RunCalls()
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	if calls[0].EnvOverlay["FOO"] != "bar" || calls[0].EnvOverlay["BAZ"] != "qux" {
		t.Fatalf("env not captured: %+v", calls[0].EnvOverlay)
	}
	// Caller mutating the original map must not affect the captured copy.
	env["FOO"] = "tampered"
	if mt.RunCalls()[0].EnvOverlay["FOO"] != "bar" {
		t.Fatal("captured env was not defensively copied")
	}
}

func TestMockTransport_UploadAndRemoveCaptured(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{}
	ctx := context.Background()

	if err := mt.Upload(ctx, "/etc/foo", []byte("payload")); err != nil {
		t.Fatal(err)
	}
	if err := mt.Upload(ctx, "/etc/bar", []byte("more")); err != nil {
		t.Fatal(err)
	}
	if err := mt.Remove(ctx, "/etc/foo"); err != nil {
		t.Fatal(err)
	}

	ups := mt.Uploaded()
	if len(ups) != 2 {
		t.Fatalf("want 2 uploads, got %d", len(ups))
	}
	if ups[0].Path != "/etc/foo" || string(ups[0].Data) != "payload" {
		t.Fatalf("upload[0] mismatch: %+v", ups[0])
	}
	if ups[1].Path != "/etc/bar" {
		t.Fatalf("upload[1] mismatch: %+v", ups[1])
	}
	rem := mt.Removed()
	if len(rem) != 1 || rem[0] != "/etc/foo" {
		t.Fatalf("removed mismatch: %v", rem)
	}
}

func TestMockTransport_UploadErrPropagated(t *testing.T) {
	t.Parallel()

	want := errors.New("disk full")
	mt := &provisiontest.MockTransport{UploadErr: want}
	if err := mt.Upload(context.Background(), "/x", []byte("y")); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	// Upload that errors must not be captured.
	if len(mt.Uploaded()) != 0 {
		t.Fatalf("upload that errored should not be captured")
	}
}

func TestMockTransport_RunResponseErrSupersedesExit(t *testing.T) {
	t.Parallel()

	want := errors.New("conn reset")
	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"x": {ExitCode: 0, Err: want},
		},
	}
	_, _, exit, err := mt.Run(context.Background(), "x", nil)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if exit != -1 {
		t.Fatalf("exit = %d, want -1", exit)
	}
}

func TestMockTransport_ContextCancellation(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{
			"x": {ExitCode: 0},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := mt.Run(ctx, "x", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}
	if err := mt.Upload(ctx, "/x", []byte("y")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Upload err = %v, want context.Canceled", err)
	}
	if err := mt.Remove(ctx, "/x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Remove err = %v, want context.Canceled", err)
	}
}

func TestMockTransport_CloseCount(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{}
	if err := mt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := mt.Close(); err != nil {
		t.Fatal(err)
	}
	if mt.CloseCount() != 2 {
		t.Fatalf("CloseCount=%d, want 2", mt.CloseCount())
	}
}

// Sanity: a deadlined ctx is reflected immediately on Run.
func TestMockTransport_DeadlineExceeded(t *testing.T) {
	t.Parallel()

	mt := &provisiontest.MockTransport{
		RunResponses: map[string]provisiontest.MockRunResponse{"x": {ExitCode: 0}},
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, _, _, err := mt.Run(ctx, "x", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}
