package release_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/release"
)

// githubRelease is the minimal JSON shape returned by the GitHub releases API.
type githubRelease struct {
	Assets []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

// newTestServer wires up a fake GitHub server that:
//   - GET /repos/<owner>/<repo>/releases/tags/<version> → returns a release
//     JSON with a single "manifest.yaml" asset whose URL points to /asset
//   - GET /asset → returns assetBody
func newTestServer(t *testing.T, assetBody []byte) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	var srv *httptest.Server

	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		assetURL := srv.URL + "/asset"
		rel := githubRelease{
			Assets: []githubAsset{
				{Name: "manifest.yaml", URL: assetURL},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	})

	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(assetBody)
	})

	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchManifest_CorrectURLAndReturnsBytes verifies that FetchManifest calls
// the correct GitHub API URL and returns the asset bytes unchanged.
func TestFetchManifest_CorrectURLAndReturnsBytes(t *testing.T) {
	const wantManifest = "apiVersion: apps/v1\nkind: Deployment\n"
	srv := newTestServer(t, []byte(wantManifest))

	client := release.NewTestClient(srv.URL, srv.Client())

	got, err := release.FetchManifestWith(context.Background(), "FoundryFabric", "myservice", "v1.2.3", "tok", client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != wantManifest {
		t.Errorf("manifest body: want %q, got %q", wantManifest, string(got))
	}
}

// TestFetchManifest_Non200FromAPIReturnsError verifies that a non-200 status
// from the GitHub releases endpoint causes FetchManifest to return an error.
func TestFetchManifest_Non200FromAPIReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
	}))
	t.Cleanup(srv.Close)

	client := release.NewTestClient(srv.URL, srv.Client())

	_, err := release.FetchManifestWith(context.Background(), "FoundryFabric", "myservice", "v9.9.9", "tok", client)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention 404, got: %v", err)
	}
}

// TestFetchManifest_MissingAssetReturnsError verifies that a release with no
// manifest.yaml asset causes FetchManifest to return a descriptive error.
func TestFetchManifest_MissingAssetReturnsError(t *testing.T) {
	// Release has assets but none named "manifest.yaml".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rel := githubRelease{
			Assets: []githubAsset{
				{Name: "other-asset.tar.gz", URL: "http://example.com/other"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rel)
	}))
	t.Cleanup(srv.Close)

	client := release.NewTestClient(srv.URL, srv.Client())

	_, err := release.FetchManifestWith(context.Background(), "FoundryFabric", "myservice", "v1.0.0", "tok", client)
	if err == nil {
		t.Fatal("expected error for missing manifest.yaml, got nil")
	}
	if !strings.Contains(err.Error(), "manifest.yaml") {
		t.Errorf("error should mention manifest.yaml, got: %v", err)
	}
}

// TestFetchManifest_MissingTokenReturnsError verifies that FetchManifest returns
// a clear error immediately when GITHUB_TOKEN is empty, before any network call.
func TestFetchManifest_MissingTokenReturnsError(t *testing.T) {
	// Use a client that fails if any request is made — the token check should
	// short-circuit before touching the network.
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := release.NewTestClient(srv.URL, srv.Client())

	_, err := release.FetchManifestWith(context.Background(), "FoundryFabric", "myservice", "v1.0.0", "" /*empty token*/, client)
	if err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("error should mention GITHUB_TOKEN, got: %v", err)
	}
	if called {
		t.Error("network call was made despite empty token — should have short-circuited")
	}
}

// TestFetchManifest_Non200AssetDownloadReturnsError verifies that a non-200
// status from the asset download step causes FetchManifest to return an error.
func TestFetchManifest_Non200AssetDownloadReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			// Return a valid release listing pointing to /asset.
			assetURL := "http://" + r.Host + "/asset"
			rel := githubRelease{Assets: []githubAsset{{Name: "manifest.yaml", URL: assetURL}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(rel)
			return
		}
		// The asset endpoint fails.
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client := release.NewTestClient(srv.URL, srv.Client())

	_, err := release.FetchManifestWith(context.Background(), "FoundryFabric", "myservice", "v1.0.0", "tok", client)
	if err == nil {
		t.Fatal("expected error for 500 asset download, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error should mention 500, got: %v", err)
	}
}
