// Package release provides utilities for downloading release assets from GitHub.
package release

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// HTTPClient is the interface for making HTTP requests. Tests inject a fake;
// production code uses http.DefaultClient when nil.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// testClient is an HTTPClient that rewrites the host of every request to a
// fixed base URL. This lets tests point FetchManifestWith at an httptest.Server
// without the production code needing to know about it.
type testClient struct {
	base  string
	inner HTTPClient
}

// NewTestClient returns an HTTPClient that replaces the scheme+host of every
// outgoing request with baseURL while delegating the actual I/O to inner.
// Intended for use in tests only.
func NewTestClient(baseURL string, inner HTTPClient) HTTPClient {
	return &testClient{base: baseURL, inner: inner}
}

func (c *testClient) Do(req *http.Request) (*http.Response, error) {
	base, err := url.Parse(c.base)
	if err != nil {
		return nil, fmt.Errorf("testClient: bad base URL %q: %w", c.base, err)
	}
	// Clone the request so we don't mutate the original.
	clone := req.Clone(req.Context())
	clone.URL.Scheme = base.Scheme
	clone.URL.Host = base.Host
	clone.Host = base.Host
	return c.inner.Do(clone)
}

// FetchManifest downloads manifest.yaml from the GitHub release asset for the
// given owner/repo/version. token is passed as a Bearer token and must not be
// included in any error message.
//
// The asset named "manifest.yaml" must exist in the release; if it is absent a
// descriptive error is returned. Non-200 responses from the GitHub API are
// treated as errors.
func FetchManifest(ctx context.Context, owner, repo, version, token string) ([]byte, error) {
	return FetchManifestWith(ctx, owner, repo, version, token, nil)
}

// FetchManifestWith is the injectable variant used by tests. Pass nil for
// client to use http.DefaultClient and the real GitHub API base URL.
func FetchManifestWith(ctx context.Context, owner, repo, version, token string, client HTTPClient) ([]byte, error) {
	if client == nil {
		client = http.DefaultClient
	}

	if token == "" {
		return nil, fmt.Errorf("release: GITHUB_TOKEN is required to fetch release assets")
	}

	// Step 1: List release assets via the GitHub releases API.
	apiURL := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build releases API request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("release: fetch releases metadata for %s/%s@%s: %w", owner, repo, version, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: GitHub API returned %d for %s/%s@%s", resp.StatusCode, owner, repo, version)
	}

	// Decode the release JSON to find the manifest.yaml asset download URL.
	var rel struct {
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("release: decode releases response for %s/%s@%s: %w", owner, repo, version, err)
	}

	// Locate the manifest.yaml asset.
	assetURL := ""
	for _, a := range rel.Assets {
		if a.Name == "manifest.yaml" {
			assetURL = a.URL
			break
		}
	}
	if assetURL == "" {
		return nil, fmt.Errorf("release: manifest.yaml not found in release assets for %s/%s@%s", owner, repo, version)
	}

	// Step 2: Download the asset. The GitHub Assets API requires the Accept
	// header to be set to application/octet-stream to get the raw bytes.
	dlReq, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("release: build asset download request: %w", err)
	}
	dlReq.Header.Set("Authorization", "Bearer "+token)
	dlReq.Header.Set("Accept", "application/octet-stream")
	dlReq.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	dlResp, err := client.Do(dlReq)
	if err != nil {
		return nil, fmt.Errorf("release: download manifest.yaml for %s/%s@%s: %w", owner, repo, version, err)
	}
	defer func() { _ = dlResp.Body.Close() }()

	if dlResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release: asset download returned %d for %s/%s@%s", dlResp.StatusCode, owner, repo, version)
	}

	data, err := io.ReadAll(dlResp.Body)
	if err != nil {
		return nil, fmt.Errorf("release: read manifest.yaml body for %s/%s@%s: %w", owner, repo, version, err)
	}

	return data, nil
}
