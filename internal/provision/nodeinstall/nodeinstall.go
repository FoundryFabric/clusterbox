// Package nodeinstall provides shared utilities for all providers that
// upload and execute the clusterboxnode binary on a remote host.
//
// Three providers currently use this package:
//   - baremetal — uses the data utilities; owns its own SSH Transport
//   - qemu      — uses data utilities + SSH helpers (port-forwarded loopback)
//   - hetzner   — uses data utilities + SSH helpers (Tailscale hostname)
package nodeinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ShortSHA returns the first 12 hex chars of sha256(b). 12 chars is
// plenty to disambiguate /tmp paths and keeps the path readable.
func ShortSHA(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])[:12]
}

// MapArch maps `uname -m` output to the linux arch token agentbundle
// understands.
func MapArch(unameOut string) (string, error) {
	v := strings.TrimSpace(unameOut)
	switch v {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("nodeinstall: unsupported arch %q (want x86_64/amd64 or aarch64/arm64)", v)
	}
}

// Result holds the fields extracted from clusterboxnode's JSON output.
// It carries either a success payload or an error payload.
type Result struct {
	// Success fields.
	KubeconfigYAML string
	NodeToken      string
	K3sVersion     string

	// Error fields (non-empty when install failed).
	ErrorMsg     string
	ErrorSection string
}

// IsError reports whether the result is an error-shape document.
func (r *Result) IsError() bool { return r.ErrorMsg != "" }

// AsError builds a descriptive error from an error-shape result.
func (r *Result) AsError(exit int, stderr []byte) error {
	return fmt.Errorf("install failed in section %s: %s (exit=%d, stderr=%q)",
		r.ErrorSection, r.ErrorMsg, exit, string(stderr))
}

// ParseInstallOutput decodes the JSON envelope clusterboxnode prints to
// stdout. stdout may contain non-JSON preamble (e.g. progress lines);
// we look for the first '{' and decode from there.
//
// Callers are responsible for asserting that required fields (e.g.
// KubeconfigYAML) are non-empty; ParseInstallOutput does not enforce
// role-specific requirements.
func ParseInstallOutput(stdout []byte) (*Result, error) {
	idx := -1
	for i, b := range stdout {
		if b == '{' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("no JSON document in install output (%d bytes)", len(stdout))
	}
	var env struct {
		Sections      map[string]map[string]any `json:"sections,omitempty"`
		ErrorMsg      string                    `json:"error,omitempty"`
		ErrorSection  string                    `json:"section,omitempty"`
		SectionsSoFar map[string]map[string]any `json:"sections_so_far,omitempty"`
	}
	if err := json.Unmarshal(stdout[idx:], &env); err != nil {
		return nil, fmt.Errorf("decode install JSON: %w", err)
	}
	if env.ErrorMsg != "" {
		return &Result{ErrorMsg: env.ErrorMsg, ErrorSection: env.ErrorSection}, nil
	}
	if env.Sections == nil {
		return nil, errors.New("install output missing sections key")
	}
	k3s, ok := env.Sections["k3s"]
	if !ok {
		return nil, errors.New("install output missing k3s section")
	}
	res := &Result{}
	if v, _ := k3s["k3s_version"].(string); v != "" {
		res.K3sVersion = v
	}
	if v, _ := k3s["kubeconfig_yaml"].(string); v != "" {
		res.KubeconfigYAML = v
	}
	if v, _ := k3s["node_token"].(string); v != "" {
		res.NodeToken = v
	}
	return res, nil
}

// RewriteKubeconfigServer replaces every cluster.server URL pointing at
// a loopback/unspecified address with https://host:6443 so the kubeconfig
// is usable from outside the target host.
func RewriteKubeconfigServer(in, host string) (string, error) {
	if in == "" {
		return "", errors.New("empty kubeconfig")
	}
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(in), &n); err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}
	rewriteServerURLs(&n, host)
	out, err := yaml.Marshal(&n)
	if err != nil {
		return "", fmt.Errorf("marshal kubeconfig: %w", err)
	}
	return string(out), nil
}

// rewriteServerURLs descends n and overwrites every "server" scalar
// whose URL points at a loopback/unspecified address with the public host.
func rewriteServerURLs(n *yaml.Node, host string) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if k.Value == "server" && v.Kind == yaml.ScalarNode {
				if isLoopbackServerURL(v.Value) {
					v.Value = "https://" + net.JoinHostPort(host, "6443")
				}
				continue
			}
			rewriteServerURLs(v, host)
		}
		return
	}
	for _, c := range n.Content {
		rewriteServerURLs(c, host)
	}
}

func isLoopbackServerURL(v string) bool {
	for _, prefix := range []string{
		"https://127.0.0.1:6443",
		"https://0.0.0.0:6443",
		"https://localhost:6443",
	} {
		if strings.HasPrefix(v, prefix) {
			return true
		}
	}
	return false
}

// WriteKubeconfig writes data to path with mode 0600. If path already
// exists it is overwritten and a warning is emitted to out.
func WriteKubeconfig(path, data string, out io.Writer) error {
	if _, err := os.Stat(path); err == nil {
		_, _ = fmt.Fprintf(out, "warning: overwriting existing kubeconfig at %s\n", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir kubeconfig dir: %w", err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		return fmt.Errorf("write kubeconfig: %w", err)
	}
	return nil
}
