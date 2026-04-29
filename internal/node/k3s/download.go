package k3s

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// k3sBinaryURL returns the GitHub releases URL for the k3s binary.
// "+" in the version is URL-encoded as "%2B".
func k3sBinaryURL(version, arch string) string {
	ver := strings.ReplaceAll(version, "+", "%2B")
	name := "k3s"
	if arch == "arm64" {
		name = "k3s-arm64"
	}
	return "https://github.com/k3s-io/k3s/releases/download/" + ver + "/" + name
}

type httpStatusErr struct{ code int }

func (e *httpStatusErr) Error() string { return fmt.Sprintf("HTTP %d", e.code) }

func isRetryableDownloadErr(err error) bool {
	var he *httpStatusErr
	if errors.As(err, &he) {
		return he.code >= 500
	}
	return true // network errors are retryable
}

// httpDownloadWithRetry downloads url to dest with exponential back-off retry.
// Writes atomically via a .tmp sibling so a partial download never replaces a
// working binary.
func httpDownloadWithRetry(ctx context.Context, url, dest string, perm os.FileMode) error {
	const maxAttempts = 5
	delay := 2 * time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			if delay < 30*time.Second {
				delay *= 2
			}
		}
		err := httpDownloadOnce(ctx, url, dest, perm)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableDownloadErr(err) {
			return err
		}
	}
	return fmt.Errorf("download %s after %d attempts: %w", url, maxAttempts, lastErr)
}

func httpDownloadOnce(ctx context.Context, url, dest string, perm os.FileMode) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusErr{code: resp.StatusCode}
	}
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return os.Rename(tmp, dest)
}
