// Package provisiontest provides hermetic test doubles for the baremetal
// transport. T7b, T9, and T11a all consume MockTransport directly so their
// tests don't need a real sshd.
package provisiontest

import (
	"context"
	"fmt"
	"sync"
)

// MockRunResponse is the canned reply for a single command match.
type MockRunResponse struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Err, if non-nil, is returned as the transport-level error and
	// supersedes ExitCode. Use this to simulate connection drops or
	// context cancellation.
	Err error
}

// MockUpload is one captured Upload call.
type MockUpload struct {
	Path string
	Data []byte
	// EnvOverlayKeys is unused for Upload but reserved to keep the struct
	// stable across variants.
}

// MockRunCall is one captured Run call. Tests typically inspect
// .RunCalls to assert on what the system-under-test executed.
type MockRunCall struct {
	Cmd          string
	EnvOverlay   map[string]string
	StdoutBytes  []byte // nil unless RunResponses provided one
	StderrBytes  []byte
	ExitCode     int
	TransportErr error
}

// MockTransport is a hermetic implementation of baremetal.Transport. It
// matches commands by exact string equality against RunResponses; an
// unmapped command is a hard error (callers' tests should fail loudly).
//
// MockTransport is safe for concurrent use.
type MockTransport struct {
	// RunResponses maps an exact cmd string to its canned response. If
	// a command is run that is not present here, Run returns an error.
	RunResponses map[string]MockRunResponse

	// UploadErr, if non-nil, is returned by every Upload call.
	UploadErr error
	// RemoveErr, if non-nil, is returned by every Remove call.
	RemoveErr error
	// CloseErr, if non-nil, is returned by Close.
	CloseErr error

	mu       sync.Mutex
	runCalls []MockRunCall
	uploaded []MockUpload
	removed  []string
	closeCnt int
}

// Run satisfies baremetal.Transport.
func (m *MockTransport) Run(ctx context.Context, cmd string, envOverlay map[string]string) ([]byte, []byte, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := ctx.Err(); err != nil {
		m.runCalls = append(m.runCalls, MockRunCall{
			Cmd:          cmd,
			EnvOverlay:   copyEnv(envOverlay),
			TransportErr: err,
			ExitCode:     -1,
		})
		return nil, nil, -1, err
	}

	resp, ok := m.RunResponses[cmd]
	if !ok {
		err := fmt.Errorf("provisiontest: MockTransport.Run: no canned response for %q", cmd)
		m.runCalls = append(m.runCalls, MockRunCall{
			Cmd:          cmd,
			EnvOverlay:   copyEnv(envOverlay),
			TransportErr: err,
			ExitCode:     -1,
		})
		return nil, nil, -1, err
	}

	m.runCalls = append(m.runCalls, MockRunCall{
		Cmd:          cmd,
		EnvOverlay:   copyEnv(envOverlay),
		StdoutBytes:  cloneBytes(resp.Stdout),
		StderrBytes:  cloneBytes(resp.Stderr),
		ExitCode:     resp.ExitCode,
		TransportErr: resp.Err,
	})
	if resp.Err != nil {
		return cloneBytes(resp.Stdout), cloneBytes(resp.Stderr), -1, resp.Err
	}
	return cloneBytes(resp.Stdout), cloneBytes(resp.Stderr), resp.ExitCode, nil
}

// Upload satisfies baremetal.Transport.
func (m *MockTransport) Upload(ctx context.Context, remotePath string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.UploadErr != nil {
		return m.UploadErr
	}
	m.uploaded = append(m.uploaded, MockUpload{
		Path: remotePath,
		Data: cloneBytes(data),
	})
	return nil
}

// Remove satisfies baremetal.Transport.
func (m *MockTransport) Remove(ctx context.Context, remotePath string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.RemoveErr != nil {
		return m.RemoveErr
	}
	m.removed = append(m.removed, remotePath)
	return nil
}

// Close satisfies baremetal.Transport.
func (m *MockTransport) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closeCnt++
	return m.CloseErr
}

// RunCalls returns a snapshot of every Run call observed so far.
func (m *MockTransport) RunCalls() []MockRunCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockRunCall, len(m.runCalls))
	copy(out, m.runCalls)
	return out
}

// Uploaded returns a snapshot of all Upload calls.
func (m *MockTransport) Uploaded() []MockUpload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]MockUpload, len(m.uploaded))
	copy(out, m.uploaded)
	return out
}

// Removed returns a snapshot of every path passed to Remove.
func (m *MockTransport) Removed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.removed))
	copy(out, m.removed)
	return out
}

// CloseCount returns the number of times Close has been called.
func (m *MockTransport) CloseCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closeCnt
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

func copyEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
