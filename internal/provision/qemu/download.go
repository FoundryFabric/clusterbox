package qemu

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// ubuntuImageBase is the base URL for Ubuntu 22.04 (Jammy) cloud images.
const ubuntuImageBase = "https://cloud-images.ubuntu.com/jammy/current"

// EnsureBaseImage downloads the Ubuntu 22.04 cloud image for the host arch
// into cacheDir if not already present. Shows progress on out.
// Returns the absolute path to the cached image.
func EnsureBaseImage(ctx context.Context, cacheDir string, out io.Writer) (string, error) {
	arch := runtime.GOARCH
	imgName, err := imageNameForArch(arch)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("qemu: mkdir cache dir: %w", err)
	}

	dest := filepath.Join(cacheDir, imgName)
	if _, err := os.Stat(dest); err == nil {
		_, _ = fmt.Fprintf(out, "qemu: base image cached at %s\n", dest)
		return dest, nil
	}

	url := fmt.Sprintf("%s/%s", ubuntuImageBase, imgName)
	_, _ = fmt.Fprintf(out, "qemu: downloading base image from %s...\n", url)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("qemu: build download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("qemu: download base image: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("qemu: download base image: HTTP %d from %s", resp.StatusCode, url)
	}

	// Atomic write: download to temp file then rename.
	tmp := dest + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return "", fmt.Errorf("qemu: create temp file: %w", err)
	}

	written, err := io.Copy(f, &progressReader{r: resp.Body, out: out, total: resp.ContentLength})
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", fmt.Errorf("qemu: write base image: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("qemu: close temp file: %w", err)
	}

	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("qemu: rename base image: %w", err)
	}

	_, _ = fmt.Fprintf(out, "qemu: base image downloaded (%d bytes) -> %s\n", written, dest)
	return dest, nil
}

// imageNameForArch returns the Ubuntu cloud image filename for the given
// Go GOARCH value.
func imageNameForArch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "jammy-server-cloudimg-amd64.img", nil
	case "arm64":
		return "jammy-server-cloudimg-arm64.img", nil
	default:
		return "", fmt.Errorf("qemu: unsupported host arch %q (want amd64 or arm64)", goarch)
	}
}

// progressReader wraps an io.Reader and writes periodic progress to out.
type progressReader struct {
	r       io.Reader
	out     io.Writer
	total   int64
	read    int64
	lastPct int
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	p.read += int64(n)
	if p.total > 0 {
		pct := int(p.read * 100 / p.total)
		if pct/10 > p.lastPct/10 {
			_, _ = fmt.Fprintf(p.out, "qemu: download progress: %d%%\n", pct)
			p.lastPct = pct
		}
	}
	return n, err
}
