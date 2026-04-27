package baremetal

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// ===== unit-level tests (no sshd) ============================================

type recordingSetenv struct {
	calls   [][2]string
	rejectK map[string]bool // names that should return an error
}

func (r *recordingSetenv) Setenv(name, value string) error {
	r.calls = append(r.calls, [2]string{name, value})
	if r.rejectK[name] {
		return errors.New("setenv rejected")
	}
	return nil
}

func TestBuildSudoCmd_NoEnv(t *testing.T) {
	t.Parallel()
	got, fb, err := buildSudoCmd(&recordingSetenv{}, "ls /tmp", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if fb {
		t.Fatal("no env -> no fallback expected")
	}
	want := "sudo -n -- /bin/sh -c 'ls /tmp'"
	if got != want {
		t.Fatalf("cmd = %q, want %q", got, want)
	}
}

func TestBuildSudoCmd_SetenvAcceptedUsesPreserveEnv(t *testing.T) {
	t.Parallel()
	r := &recordingSetenv{}
	got, fb, err := buildSudoCmd(r, "id", map[string]string{
		"FOO": "bar",
		"BAZ": "qux",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if fb {
		t.Fatal("Setenv accepted, fallback should be false")
	}
	// Keys are sorted: BAZ, FOO.
	wantPrefix := "sudo -n --preserve-env=BAZ,FOO -- /bin/sh -c "
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("cmd = %q, want prefix %q", got, wantPrefix)
	}
	// Values must NOT appear in the command line.
	if strings.Contains(got, "bar") || strings.Contains(got, "qux") {
		t.Fatalf("env values leaked into cmd line: %q", got)
	}
	// Both Setenv calls observed.
	if len(r.calls) != 2 {
		t.Fatalf("Setenv calls = %d, want 2", len(r.calls))
	}
}

func TestBuildSudoCmd_SetenvRejectedFallsBack(t *testing.T) {
	t.Parallel()
	r := &recordingSetenv{rejectK: map[string]bool{"BAZ": true}}
	got, fb, err := buildSudoCmd(r, "id", map[string]string{
		"FOO": "bar",
		"BAZ": "qux secret",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !fb {
		t.Fatal("Setenv rejected -> fallback expected")
	}
	// Should pass values positionally, properly quoted.
	if !strings.Contains(got, "BAZ='qux secret'") {
		t.Fatalf("expected positional BAZ='qux secret' in %q", got)
	}
	if !strings.Contains(got, "FOO=bar") {
		t.Fatalf("expected FOO=bar in %q", got)
	}
}

func TestBuildSudoCmd_ForceFallbackSkipsSetenv(t *testing.T) {
	t.Parallel()
	r := &recordingSetenv{}
	got, fb, err := buildSudoCmd(r, "id", map[string]string{"K": "v"}, true)
	if err != nil {
		t.Fatal(err)
	}
	if !fb {
		t.Fatal("forceFallback should report fallbackTriggered=true")
	}
	if len(r.calls) != 0 {
		t.Fatalf("forceFallback should not call Setenv, got %d calls", len(r.calls))
	}
	if !strings.Contains(got, "sudo -n K=v -- /bin/sh -c") {
		t.Fatalf("unexpected cmd: %q", got)
	}
}

func TestShellQuote(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                "''",
		"plain":           "plain",
		"with space":      `'with space'`,
		"with'apos":       `'with'\''apos'`,
		"/usr/bin/foo":    "/usr/bin/foo",
		"a$b":             `'a$b'`,
		"path-1.2_3:4,5+": "path-1.2_3:4,5+",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

// ===== hermetic sshd integration ============================================

// generateTestKey writes a fresh ed25519 OpenSSH-format private key to disk
// and returns its path plus the corresponding ssh.Signer (for the server).
func generateTestKey(t *testing.T) (privPath string, signer ssh.Signer) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err = ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(privPath, pem.EncodeToMemory(pemBytes), 0600); err != nil {
		t.Fatal(err)
	}
	_ = pub
	return privPath, signer
}

// fakeSession is a request fed to mockHandler.
type fakeSession struct {
	cmd        string
	envSet     map[string]string
	envRejects map[string]bool // server rejects SetEnv for these names
}

// mockHandler is the "what does the server do for this exec?" hook. The
// handler may read from stdin (e.g. for upload commands that pipe data).
type mockHandler func(s *fakeSession, stdin io.Reader) (stdout, stderr []byte, exitCode uint32)

// startMockSSHD spawns an in-process sshd backed by net.Listen on 127.0.0.1:0.
// The returned addr is host:port; cleanup stops the listener and waits for
// goroutines.
//
// envRejectKeys are the env var names the server rejects ("env" SSH request
// reply=false). The server always permits "exec" requests.
func startMockSSHD(t *testing.T, hostKey ssh.Signer, clientPub ssh.PublicKey, envRejectKeys map[string]bool, handler mockHandler) string {
	t.Helper()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(c ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(clientPub.Marshal()) {
				return &ssh.Permissions{}, nil
			}
			return nil, errors.New("unauthorized")
		},
	}
	cfg.AddHostKey(hostKey)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			nc, err := lis.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				serveOne(t, nc, cfg, envRejectKeys, handler)
			}()
		}
	}()

	t.Cleanup(func() {
		_ = lis.Close()
		wg.Wait()
	})
	return lis.Addr().String()
}

func serveOne(t *testing.T, nc net.Conn, cfg *ssh.ServerConfig, envRejects map[string]bool, handler mockHandler) {
	t.Helper()
	defer func() { _ = nc.Close() }()
	conn, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			_ = newCh.Reject(ssh.UnknownChannelType, "no")
			continue
		}
		ch, requests, err := newCh.Accept()
		if err != nil {
			continue
		}
		go handleSession(ch, requests, envRejects, handler)
	}
}

func handleSession(ch ssh.Channel, requests <-chan *ssh.Request, envRejects map[string]bool, handler mockHandler) {
	defer func() { _ = ch.Close() }()
	sess := &fakeSession{envSet: map[string]string{}, envRejects: envRejects}
	for req := range requests {
		switch req.Type {
		case "env":
			var msg struct{ Name, Value string }
			_ = ssh.Unmarshal(req.Payload, &msg)
			if envRejects[msg.Name] {
				_ = req.Reply(false, nil)
				continue
			}
			sess.envSet[msg.Name] = msg.Value
			_ = req.Reply(true, nil)
		case "exec":
			var msg struct{ Cmd string }
			_ = ssh.Unmarshal(req.Payload, &msg)
			sess.cmd = msg.Cmd
			_ = req.Reply(true, nil)

			stdout, stderr, exit := handler(sess, ch)
			if len(stdout) > 0 {
				_, _ = ch.Write(stdout)
			}
			if len(stderr) > 0 {
				_, _ = ch.Stderr().Write(stderr)
			}
			// Send exit-status. Payload per RFC 4254 §6.10 is uint32 BE.
			payload := make([]byte, 4)
			payload[0] = byte(exit >> 24)
			payload[1] = byte(exit >> 16)
			payload[2] = byte(exit >> 8)
			payload[3] = byte(exit)
			_, _ = ch.SendRequest("exit-status", false, payload)
			_ = ch.Close()
			return
		default:
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

// dialTest is a helper that uses InsecureIgnoreHostKey for these in-process
// tests. Production callers should always pin a host key.
func dialTest(t *testing.T, addr, keyPath string) Transport {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tr, err := Dial(ctx, DialConfig{
		Host:            addr,
		User:            "tester",
		SSHKeyPath:      keyPath,
		Timeout:         5 * time.Second,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = tr.Close() })
	return tr
}

func loadPubKey(t *testing.T, privPath string) ssh.PublicKey {
	t.Helper()
	b, err := os.ReadFile(privPath)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.ParsePrivateKey(b)
	if err != nil {
		t.Fatal(err)
	}
	return signer.PublicKey()
}

func TestDialAndRun_Basic(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)
	clientPub := loadPubKey(t, keyPath)

	addr := startMockSSHD(t, hostKey, clientPub, nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		switch {
		case s.cmd == "sudo -n true":
			return nil, nil, 0
		case strings.HasPrefix(s.cmd, "sudo -n -- /bin/sh -c"):
			return []byte("ok\n"), nil, 0
		}
		return nil, []byte("unknown: " + s.cmd), 127
	})

	tr := dialTest(t, addr, keyPath)
	stdout, _, exit, err := tr.Run(context.Background(), "echo ok", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	if string(stdout) != "ok\n" {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestRun_SudoNotPasswordless(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)
	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		// Reply non-zero to the sudo probe — simulates a password-required sudo.
		if s.cmd == "sudo -n true" {
			return nil, []byte("a password is required\n"), 1
		}
		t.Errorf("unexpected exec %q after sudo probe failed", s.cmd)
		return nil, nil, 1
	})

	tr := dialTest(t, addr, keyPath)
	_, _, _, err := tr.Run(context.Background(), "ls", nil)
	if !errors.Is(err, ErrSudoNotPasswordless) {
		t.Fatalf("err = %v, want ErrSudoNotPasswordless", err)
	}
	// Subsequent calls should also short-circuit without re-probing.
	_, _, _, err = tr.Run(context.Background(), "ls", nil)
	if !errors.Is(err, ErrSudoNotPasswordless) {
		t.Fatalf("second Run err = %v", err)
	}
}

func TestRun_SetenvAcceptedNoLeakInCmdLine(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)

	const secret = "topsecret-value"
	var (
		mu                sync.Mutex
		observedExecCmd   string
		observedSetenv    map[string]string
		sawPreserveEnvOpt bool
	)

	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		mu.Lock()
		defer mu.Unlock()
		if s.cmd == "sudo -n true" {
			return nil, nil, 0
		}
		observedExecCmd = s.cmd
		observedSetenv = map[string]string{}
		for k, v := range s.envSet {
			observedSetenv[k] = v
		}
		if strings.Contains(s.cmd, "--preserve-env=") {
			sawPreserveEnvOpt = true
		}
		return []byte("done"), nil, 0
	})

	tr := dialTest(t, addr, keyPath)
	_, _, exit, err := tr.Run(context.Background(), "id", map[string]string{"SECRET": secret})
	if err != nil || exit != 0 {
		t.Fatalf("Run err=%v exit=%d", err, exit)
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawPreserveEnvOpt {
		t.Fatalf("expected --preserve-env=SECRET on cmd, got %q", observedExecCmd)
	}
	if strings.Contains(observedExecCmd, secret) {
		t.Fatalf("secret value leaked into cmd line: %q", observedExecCmd)
	}
	if observedSetenv["SECRET"] != secret {
		t.Fatalf("server did not see SECRET via SSH env req; got %v", observedSetenv)
	}
}

func TestRun_SetenvRejectedFallsBack(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)

	rejects := map[string]bool{"SECRET": true}
	var (
		mu             sync.Mutex
		execCmd        string
		setenvObserved map[string]string
	)

	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), rejects, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		mu.Lock()
		defer mu.Unlock()
		if s.cmd == "sudo -n true" {
			return nil, nil, 0
		}
		execCmd = s.cmd
		setenvObserved = map[string]string{}
		for k, v := range s.envSet {
			setenvObserved[k] = v
		}
		return nil, nil, 0
	})

	tr := dialTest(t, addr, keyPath)
	_, _, exit, err := tr.Run(context.Background(), "id", map[string]string{"SECRET": "shh"})
	if err != nil || exit != 0 {
		t.Fatalf("Run err=%v exit=%d", err, exit)
	}

	mu.Lock()
	defer mu.Unlock()
	// Server rejected env -> client falls back to inline sudo arg.
	if !strings.Contains(execCmd, "SECRET=") {
		t.Fatalf("expected positional SECRET=... in fallback cmd, got %q", execCmd)
	}
	if _, ok := setenvObserved["SECRET"]; ok {
		t.Fatalf("rejected SECRET should not be in server's set env, got %v", setenvObserved)
	}

	// Second Run should skip Setenv entirely (sticky fallback).
	stTr := tr.(*sshTransport)
	stTr.mu.Lock()
	stuck := stTr.useEnvFallback
	stTr.mu.Unlock()
	if !stuck {
		t.Fatal("expected sticky fallback after rejection")
	}
}

func TestUploadAndRemove(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)

	var (
		mu          sync.Mutex
		uploadedTo  string
		uploadData  []byte
		chmodCmd    string
		removedPath string
	)

	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		mu.Lock()
		defer mu.Unlock()
		if s.cmd == "sudo -n true" {
			return nil, nil, 0
		}
		// Upload's exec command.
		if strings.Contains(s.cmd, "umask 077 && cat >") {
			// Match the path portion: ...cat > /tmp/foo
			i := strings.Index(s.cmd, "cat > ")
			if i < 0 {
				return nil, []byte("bad upload cmd"), 1
			}
			pathPart := s.cmd[i+len("cat > "):]
			pathPart = strings.TrimSuffix(pathPart, "'")
			uploadedTo = pathPart
			// Drain stdin so the client's pipe write completes.
			data, _ := io.ReadAll(stdin)
			uploadData = data
			return nil, nil, 0
		}
		if strings.Contains(s.cmd, "chmod 0700") {
			chmodCmd = s.cmd
			return nil, nil, 0
		}
		if strings.Contains(s.cmd, "rm -f") {
			removedPath = s.cmd
			return nil, nil, 0
		}
		return nil, []byte("unknown"), 127
	})

	tr := dialTest(t, addr, keyPath)
	if err := tr.Upload(context.Background(), "/etc/clusterbox/config", []byte("hello")); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if err := tr.Remove(context.Background(), "/etc/clusterbox/config"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(uploadedTo, "/etc/clusterbox/config") {
		t.Fatalf("upload path not seen: %q", uploadedTo)
	}
	if !strings.Contains(chmodCmd, "chmod 0700") || !strings.Contains(chmodCmd, "chown root:root") {
		t.Fatalf("chmod cmd not seen: %q", chmodCmd)
	}
	if !strings.Contains(removedPath, "rm -f") || !strings.Contains(removedPath, "/etc/clusterbox/config") {
		t.Fatalf("remove cmd not seen: %q", removedPath)
	}

	if string(uploadData) != "hello" {
		t.Fatalf("upload data mismatch: got %q, want %q", uploadData, "hello")
	}
}

func TestRun_NonZeroExitNotReturnedAsError(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)
	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		if s.cmd == "sudo -n true" {
			return nil, nil, 0
		}
		return []byte("oops\n"), []byte("nope\n"), 42
	})
	tr := dialTest(t, addr, keyPath)
	stdout, stderr, exit, err := tr.Run(context.Background(), "false", nil)
	if err != nil {
		t.Fatalf("Run err = %v, want nil for non-zero remote exit", err)
	}
	if exit != 42 {
		t.Fatalf("exit = %d, want 42", exit)
	}
	if string(stdout) != "oops\n" || string(stderr) != "nope\n" {
		t.Fatalf("stdout=%q stderr=%q", stdout, stderr)
	}
}

func TestDial_BadKeyPath(t *testing.T) {
	t.Parallel()
	_, err := Dial(context.Background(), DialConfig{
		Host:       "127.0.0.1:1",
		User:       "x",
		SSHKeyPath: "/nonexistent/key/path",
	})
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestDial_HostUserKeyRequired(t *testing.T) {
	t.Parallel()
	for _, c := range []DialConfig{
		{},
		{Host: "x"},
		{Host: "x", User: "u"},
	} {
		if _, err := Dial(context.Background(), c); err == nil {
			t.Fatalf("expected error for cfg=%+v", c)
		}
	}
}

func TestClose_Idempotent(t *testing.T) {
	t.Parallel()
	keyPath, hostKey := generateTestKey(t)
	addr := startMockSSHD(t, hostKey, loadPubKey(t, keyPath), nil, func(s *fakeSession, stdin io.Reader) ([]byte, []byte, uint32) {
		return nil, nil, 0
	})
	tr := dialTest(t, addr, keyPath)
	if err := tr.Close(); err != nil {
		t.Fatal(err)
	}
	// Second Close must be a no-op.
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}
