// Package baremetal provides transport primitives (ssh, file upload, remote
// exec) used by the bare-metal provisioner. It deliberately contains no
// provisioning business logic; callers compose these helpers.
package baremetal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// Transport is the abstract remote-execution surface used by the bare-metal
// provisioner. Implementations must be safe for sequential use by a single
// goroutine; concurrent calls on the same Transport are not supported.
type Transport interface {
	// Run executes cmd over ssh as root via sudo. envOverlay is set on the
	// remote process: the implementation tries SSH "env" requests first and
	// falls back to positional `KEY=VALUE` arguments on the sudo command
	// line if the server rejects them.
	//
	// The returned stdout/stderr never contain the values of envOverlay
	// from this function's own bookkeeping — only what the remote command
	// itself prints. Production-grade callers must still avoid echoing
	// secrets in the remote command's own logs.
	Run(ctx context.Context, cmd string, envOverlay map[string]string) (stdout, stderr []byte, exitCode int, err error)

	// Upload writes data to remotePath on the target with mode 0700 and
	// owner=root.
	Upload(ctx context.Context, remotePath string, data []byte) error

	// Remove deletes remotePath best-effort. Missing files are not an error.
	Remove(ctx context.Context, remotePath string) error

	Close() error
}

// DialConfig configures a Dial.
type DialConfig struct {
	// Host is the target host:port. If no port is present, ":22" is appended.
	Host string
	// User is the SSH login user. The remote process is escalated via sudo.
	User string
	// SSHKeyPath is the path to a PEM-encoded private key.
	SSHKeyPath string
	// Timeout bounds the TCP+handshake establishment.
	Timeout time.Duration
	// HostKeyCallback, if non-nil, validates the server host key. If nil,
	// ssh.InsecureIgnoreHostKey() is used. Production callers should always
	// supply a verifying callback.
	HostKeyCallback ssh.HostKeyCallback
}

// ErrSudoNotPasswordless is returned when the configured user cannot run
// `sudo -n true` on the target.
var ErrSudoNotPasswordless = errors.New("baremetal: user cannot sudo without a password")

// sshTransport is the production Transport. It owns a single ssh.Client.
type sshTransport struct {
	client *ssh.Client

	mu             sync.Mutex
	sudoChecked    bool
	sudoOK         bool
	useEnvFallback bool // sticky: once Setenv is rejected, skip it for the rest of the connection
}

// Dial establishes an ssh connection to cfg.Host using the private key at
// cfg.SSHKeyPath. The caller owns the returned Transport and must Close it.
//
// On any error after the TCP connection is established, internal resources
// are released before returning.
func Dial(ctx context.Context, cfg DialConfig) (_ Transport, retErr error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("baremetal: DialConfig.Host required")
	}
	if cfg.User == "" {
		return nil, fmt.Errorf("baremetal: DialConfig.User required")
	}
	if cfg.SSHKeyPath == "" {
		return nil, fmt.Errorf("baremetal: DialConfig.SSHKeyPath required")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}

	keyBytes, err := os.ReadFile(cfg.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("baremetal: read ssh key %q: %w", cfg.SSHKeyPath, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("baremetal: parse ssh key %q: %w", cfg.SSHKeyPath, err)
	}

	host := cfg.Host
	if _, _, splitErr := net.SplitHostPort(host); splitErr != nil {
		host = net.JoinHostPort(host, "22")
	}

	hostKeyCb := cfg.HostKeyCallback
	if hostKeyCb == nil {
		hostKeyCb = ssh.InsecureIgnoreHostKey()
	}

	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCb,
		Timeout:         cfg.Timeout,
	}

	dialer := &net.Dialer{Timeout: cfg.Timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return nil, fmt.Errorf("baremetal: dial %s: %w", host, err)
	}
	defer func() {
		// On any error before NewClientConn takes ownership, close the raw
		// conn. After NewClientConn succeeds we set rawConn=nil so this is
		// a no-op.
		if retErr != nil && rawConn != nil {
			_ = rawConn.Close()
		}
	}()

	if d, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(d)
	}

	c, chans, reqs, err := ssh.NewClientConn(rawConn, host, clientCfg)
	if err != nil {
		return nil, fmt.Errorf("baremetal: ssh handshake to %s: %w", host, err)
	}
	// Clear the deadline so it does not bleed into post-handshake reads
	// (subsequent Run calls supply their own context deadlines).
	_ = rawConn.SetDeadline(time.Time{})
	// NewClientConn took ownership of the underlying conn.
	rawConn = nil

	client := ssh.NewClient(c, chans, reqs)
	return &sshTransport{client: client}, nil
}

// Close releases the underlying ssh.Client.
func (t *sshTransport) Close() error {
	if t.client == nil {
		return nil
	}
	err := t.client.Close()
	t.client = nil
	return err
}

// Run satisfies Transport.
func (t *sshTransport) Run(ctx context.Context, cmd string, envOverlay map[string]string) ([]byte, []byte, int, error) {
	if t.client == nil {
		return nil, nil, -1, errors.New("baremetal: Run on closed Transport")
	}
	if err := t.ensureSudoNoPassword(ctx); err != nil {
		return nil, nil, -1, err
	}

	sess, err := t.client.NewSession()
	if err != nil {
		return nil, nil, -1, fmt.Errorf("baremetal: new session: %w", err)
	}
	defer func() { _ = sess.Close() }()

	t.mu.Lock()
	useFallback := t.useEnvFallback
	t.mu.Unlock()

	cmdLine, fbTriggered, err := buildSudoCmd(sess, cmd, envOverlay, useFallback)
	if err != nil {
		return nil, nil, -1, err
	}
	if fbTriggered && !useFallback {
		t.mu.Lock()
		t.useEnvFallback = true
		t.mu.Unlock()
	}

	return runSession(ctx, sess, cmdLine)
}

// Upload satisfies Transport. The data is streamed through `sudo cat > path`
// so the resulting file is created by root. mode 0700 is enforced explicitly
// after the write completes.
func (t *sshTransport) Upload(ctx context.Context, remotePath string, data []byte) error {
	if t.client == nil {
		return errors.New("baremetal: Upload on closed Transport")
	}
	if remotePath == "" {
		return errors.New("baremetal: Upload requires remotePath")
	}
	if err := t.ensureSudoNoPassword(ctx); err != nil {
		return err
	}

	sess, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("baremetal: new session for upload: %w", err)
	}
	defer func() { _ = sess.Close() }()

	stdin, err := sess.StdinPipe()
	if err != nil {
		return fmt.Errorf("baremetal: stdin pipe: %w", err)
	}
	var stderr bytes.Buffer
	sess.Stderr = &stderr
	sess.Stdout = io.Discard

	cmd := fmt.Sprintf("sudo -n -- /bin/sh -c %s",
		shellQuote(fmt.Sprintf("umask 077 && cat > %s", shellQuote(remotePath))))

	if err := sess.Start(cmd); err != nil {
		return fmt.Errorf("baremetal: start upload: %w", err)
	}

	writeErrCh := make(chan error, 1)
	go func() {
		_, werr := stdin.Write(data)
		closeErr := stdin.Close()
		if werr != nil {
			writeErrCh <- werr
			return
		}
		writeErrCh <- closeErr
	}()

	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return ctx.Err()
	case werr := <-waitErrCh:
		if wErr := <-writeErrCh; wErr != nil && werr == nil {
			werr = wErr
		}
		if werr != nil {
			return fmt.Errorf("baremetal: upload %s: %w (stderr=%q)", remotePath, werr, stderr.String())
		}
	}

	// Enforce mode 0700 + root ownership explicitly.
	chmodCmd := fmt.Sprintf("sudo -n -- /bin/sh -c %s",
		shellQuote(fmt.Sprintf("chmod 0700 %s && chown root:root %s",
			shellQuote(remotePath), shellQuote(remotePath))))

	chmodSess, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("baremetal: new session for chmod: %w", err)
	}
	defer func() { _ = chmodSess.Close() }()
	_, _, exit, err := runSession(ctx, chmodSess, chmodCmd)
	if err != nil {
		return fmt.Errorf("baremetal: chmod/chown %s: %w", remotePath, err)
	}
	if exit != 0 {
		return fmt.Errorf("baremetal: chmod/chown %s exit=%d", remotePath, exit)
	}
	return nil
}

// Remove satisfies Transport.
func (t *sshTransport) Remove(ctx context.Context, remotePath string) error {
	if t.client == nil {
		return errors.New("baremetal: Remove on closed Transport")
	}
	if remotePath == "" {
		return errors.New("baremetal: Remove requires remotePath")
	}
	if err := t.ensureSudoNoPassword(ctx); err != nil {
		return err
	}
	sess, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("baremetal: new session for remove: %w", err)
	}
	defer func() { _ = sess.Close() }()
	cmd := fmt.Sprintf("sudo -n -- /bin/sh -c %s",
		shellQuote("rm -f -- "+shellQuote(remotePath)))
	_, _, _, err = runSession(ctx, sess, cmd)
	return err
}

// ensureSudoNoPassword runs `sudo -n true` once per Transport lifetime. On
// failure the transport is permanently marked sudo-unusable and
// ErrSudoNotPasswordless is returned for every subsequent Run/Upload/Remove.
func (t *sshTransport) ensureSudoNoPassword(ctx context.Context) error {
	t.mu.Lock()
	checked, ok := t.sudoChecked, t.sudoOK
	t.mu.Unlock()
	if checked {
		if !ok {
			return ErrSudoNotPasswordless
		}
		return nil
	}

	sess, err := t.client.NewSession()
	if err != nil {
		return fmt.Errorf("baremetal: new session for sudo probe: %w", err)
	}
	defer func() { _ = sess.Close() }()
	_, _, exit, runErr := runSession(ctx, sess, "sudo -n true")

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sudoChecked = true
	if runErr != nil || exit != 0 {
		t.sudoOK = false
		return ErrSudoNotPasswordless
	}
	t.sudoOK = true
	return nil
}

// runSession runs cmdLine on the given (already-prepared) session, returning
// stdout/stderr captures, the remote exit code, and any non-exit error
// (transport, ctx, etc). A non-zero remote exit is reported via exitCode,
// not via err — matching the Transport.Run contract.
func runSession(ctx context.Context, sess *ssh.Session, cmdLine string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	if sess.Stdout == nil {
		sess.Stdout = &stdout
	}
	if sess.Stderr == nil {
		sess.Stderr = &stderr
	}

	if err := sess.Start(cmdLine); err != nil {
		return nil, nil, -1, fmt.Errorf("baremetal: start command: %w", err)
	}

	waitErrCh := make(chan error, 1)
	go func() { waitErrCh <- sess.Wait() }()

	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		_ = sess.Close()
		return stdout.Bytes(), stderr.Bytes(), -1, ctx.Err()
	case werr := <-waitErrCh:
		if werr != nil {
			var ee *ssh.ExitError
			if errors.As(werr, &ee) {
				return stdout.Bytes(), stderr.Bytes(), ee.ExitStatus(), nil
			}
			return stdout.Bytes(), stderr.Bytes(), -1, fmt.Errorf("baremetal: wait command: %w", werr)
		}
		return stdout.Bytes(), stderr.Bytes(), 0, nil
	}
}
