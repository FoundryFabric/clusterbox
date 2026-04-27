package onepassword_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets/onepassword"
)

// ---- mock HTTP client ----

type mockHTTPClient struct {
	fn func(r *http.Request) (*http.Response, error)
}

func (m *mockHTTPClient) Do(req *http.Request) (*http.Response, error) {
	return m.fn(req)
}

func jsonResp(t *testing.T, code int, body interface{}) *http.Response {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader(string(b))),
		Header:     make(http.Header),
	}
}

// buildThreeCallClient serves the standard three-call Connect sequence:
// GET /v1/vaults → GET /v1/vaults/<id>/items → GET /v1/vaults/<id>/items/<id>
func buildThreeCallClient(t *testing.T, fields []map[string]interface{}) *mockHTTPClient {
	t.Helper()
	return &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			switch {
			case r.URL.Path == "/v1/vaults":
				return jsonResp(t, 200, []map[string]interface{}{
					{"id": "vault-uuid-1", "name": "foundryfabric"},
				}), nil
			case strings.HasSuffix(r.URL.Path, "/items") && !strings.Contains(r.URL.Path, "/items/"):
				return jsonResp(t, 200, []map[string]interface{}{
					{"id": "item-uuid-1", "title": "dev-hetzner-ash"},
				}), nil
			case strings.Contains(r.URL.Path, "/items/item-uuid-1"):
				return jsonResp(t, 200, map[string]interface{}{
					"id":     "item-uuid-1",
					"fields": fields,
				}), nil
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				return &http.Response{
					StatusCode: 404,
					Body:       io.NopCloser(strings.NewReader("not found")),
					Header:     make(http.Header),
				}, nil
			}
		},
	}
}

var sampleFields = []map[string]interface{}{
	{"id": "f1", "label": "JWT_SECRET", "value": "jwt-val", "purpose": ""},
	{"id": "f2", "label": "DB_PASSWORD", "value": "db-val", "purpose": ""},
	// system field — should be filtered out
	{"id": "f3", "label": "username", "value": "admin", "purpose": "USERNAME"},
}

// TestConnectMode_GetAll returns all user fields and filters system fields.
func TestConnectMode_GetAll(t *testing.T) {
	client := buildThreeCallClient(t, sampleFields)
	cfg := onepassword.Config{ConnectHost: "http://localhost:8080", ConnectToken: "token"}
	p := onepassword.NewWithClient(cfg, client)

	got, err := p.GetAll(context.Background(), "foundryfabric", "dev", "hetzner", "ash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got["JWT_SECRET"] != "jwt-val" {
		t.Errorf("JWT_SECRET: want jwt-val got %q", got["JWT_SECRET"])
	}
	if got["DB_PASSWORD"] != "db-val" {
		t.Errorf("DB_PASSWORD: want db-val got %q", got["DB_PASSWORD"])
	}
	if _, ok := got["username"]; ok {
		t.Error("system field 'username' should be filtered out")
	}
}

// TestConnectMode_Get returns a single field.
func TestConnectMode_Get(t *testing.T) {
	client := buildThreeCallClient(t, sampleFields)
	cfg := onepassword.Config{ConnectHost: "http://localhost:8080", ConnectToken: "token"}
	p := onepassword.NewWithClient(cfg, client)

	val, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "jwt-val" {
		t.Errorf("want jwt-val got %q", val)
	}
}

// TestConnectMode_CacheAvoidsDuplicateVaultListCalls verifies that a second
// Get call does not re-list vaults (UUID is cached).
func TestConnectMode_CacheAvoidsDuplicateVaultListCalls(t *testing.T) {
	vaultListCalls := 0
	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			switch {
			case r.URL.Path == "/v1/vaults":
				vaultListCalls++
				return jsonResp(t, 200, []map[string]interface{}{
					{"id": "vault-uuid-1", "name": "foundryfabric"},
				}), nil
			case strings.HasSuffix(r.URL.Path, "/items") && !strings.Contains(r.URL.Path, "/items/"):
				return jsonResp(t, 200, []map[string]interface{}{
					{"id": "item-uuid-1", "title": "dev-hetzner-ash"},
				}), nil
			case strings.Contains(r.URL.Path, "/items/item-uuid-1"):
				return jsonResp(t, 200, map[string]interface{}{
					"id": "item-uuid-1",
					"fields": []map[string]interface{}{
						{"id": "f1", "label": "JWT_SECRET", "value": "jwt-val", "purpose": ""},
					},
				}), nil
			default:
				return &http.Response{
					StatusCode: 404,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			}
		},
	}

	cfg := onepassword.Config{ConnectHost: "http://localhost:8080", ConnectToken: "token"}
	p := onepassword.NewWithClient(cfg, client)

	if _, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET"); err != nil {
		t.Fatalf("first Get: %v", err)
	}
	if _, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET"); err != nil {
		t.Fatalf("second Get: %v", err)
	}

	if vaultListCalls != 1 {
		t.Errorf("expected vault list called once, got %d", vaultListCalls)
	}
}

// TestCLIFallback_Get shells out to op read when ConnectHost is empty.
func TestCLIFallback_Get(t *testing.T) {
	var gotArgs []string
	runFn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		gotArgs = args
		return []byte("cli-secret-value\n"), nil
	}

	cfg := onepassword.Config{ServiceAccountToken: "svc-token"}
	p := onepassword.NewWithRunner(cfg, runFn)

	val, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Trailing newline must be stripped.
	if val != "cli-secret-value" {
		t.Errorf("want cli-secret-value got %q", val)
	}

	// Verify the correct op:// path was passed.
	if len(gotArgs) < 2 || gotArgs[0] != "read" {
		t.Fatalf("expected 'op read <path>', got args: %v", gotArgs)
	}
	wantPath := "op://foundryfabric/dev-hetzner-ash/JWT_SECRET"
	if gotArgs[1] != wantPath {
		t.Errorf("want op path %q got %q", wantPath, gotArgs[1])
	}
}

// TestCLIFallback_ErrorDoesNotLeakPath verifies that CLI errors do not include
// the secret path to avoid leaking credential naming conventions.
func TestCLIFallback_ErrorDoesNotLeakPath(t *testing.T) {
	runFn := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, &fakeExitError{}
	}

	cfg := onepassword.Config{ServiceAccountToken: "svc-token"}
	p := onepassword.NewWithRunner(cfg, runFn)

	_, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), "op://") {
		t.Errorf("error must not include op:// path, got: %v", err)
	}
}

// TestCLIFallback_GetAll_Success parses op item get JSON and returns user fields.
func TestCLIFallback_GetAll_Success(t *testing.T) {
	itemJSON, _ := json.Marshal(map[string]interface{}{
		"id":    "item-uuid-1",
		"title": "dev-k3d",
		"fields": []map[string]interface{}{
			{"id": "f1", "label": "GH_PAT_TOKEN", "value": "ghp_xxx", "purpose": ""},
			{"id": "f2", "label": "GH_APP_ID", "value": "12345", "purpose": ""},
			// system field — filtered out
			{"id": "f3", "label": "username", "value": "admin", "purpose": "USERNAME"},
		},
	})

	runFn := func(_ context.Context, _ string, args ...string) ([]byte, error) {
		// Verify correct arguments are passed.
		if len(args) < 5 || args[0] != "item" || args[1] != "get" {
			t.Errorf("expected 'op item get ...', got: %v", args)
		}
		return itemJSON, nil
	}

	cfg := onepassword.Config{} // no ConnectHost → CLI mode
	p := onepassword.NewWithRunner(cfg, runFn)

	got, err := p.GetAll(context.Background(), "myaddon", "dev", "k3d", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["GH_PAT_TOKEN"] != "ghp_xxx" {
		t.Errorf("GH_PAT_TOKEN: want ghp_xxx got %q", got["GH_PAT_TOKEN"])
	}
	if got["GH_APP_ID"] != "12345" {
		t.Errorf("GH_APP_ID: want 12345 got %q", got["GH_APP_ID"])
	}
	if _, ok := got["username"]; ok {
		t.Error("system field 'username' should be filtered out")
	}
}

// TestCLIFallback_GetAll_ItemNotFound returns an empty map (not an error)
// so addons with no secrets install without requiring a pre-created item.
func TestCLIFallback_GetAll_ItemNotFound(t *testing.T) {
	runFn := func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return nil, &fakeExitError{}
	}

	cfg := onepassword.Config{}
	p := onepassword.NewWithRunner(cfg, runFn)

	got, err := p.GetAll(context.Background(), "myaddon", "dev", "k3d", "")
	if err != nil {
		t.Fatalf("item-not-found should return empty map, not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty map, got %v", got)
	}
}

// TestItemTitle covers the empty-part stripping behaviour.
func TestItemTitle(t *testing.T) {
	cases := []struct {
		env, provider, region string
		want                  string
	}{
		{"dev", "k3d", "", "dev-k3d"},
		{"dev", "hetzner", "ash", "dev-hetzner-ash"},
		{"", "k3d", "", "k3d"},
		{"dev", "", "", "dev"},
		{"", "", "", ""},
	}
	for _, tc := range cases {
		got := onepassword.ItemTitle(tc.env, tc.provider, tc.region)
		if got != tc.want {
			t.Errorf("ItemTitle(%q,%q,%q) = %q, want %q", tc.env, tc.provider, tc.region, got, tc.want)
		}
	}
}

type fakeExitError struct{}

func (e *fakeExitError) Error() string { return "exit status 1" }
