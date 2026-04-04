package vault_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/secrets/vault"
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

// kvResponse builds a Vault KV v2 response envelope.
func kvResponse(t *testing.T, data map[string]string) *http.Response {
	t.Helper()
	return jsonResp(t, 200, map[string]interface{}{
		"data": map[string]interface{}{
			"data":     data,
			"metadata": map[string]interface{}{"version": 1},
		},
	})
}

// ---- token auth tests ----

func TestTokenAuth_GetAll(t *testing.T) {
	wantData := map[string]string{"JWT_SECRET": "tok-jwt", "DB_PASSWORD": "tok-db"}

	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Vault-Token") != "mytoken" {
				t.Errorf("X-Vault-Token: want mytoken got %q", r.Header.Get("X-Vault-Token"))
			}
			if !strings.Contains(r.URL.Path, "/secret/data/foundryfabric/dev/hetzner/ash") {
				t.Errorf("unexpected path: %s", r.URL.Path)
			}
			return kvResponse(t, wantData), nil
		},
	}

	cfg := vault.Config{Addr: "http://vault.test:8200", Token: "mytoken"}
	p := vault.NewWithClient(cfg, client)

	got, err := p.GetAll(context.Background(), "foundryfabric", "dev", "hetzner", "ash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for k, wv := range wantData {
		if gv := got[k]; gv != wv {
			t.Errorf("key %q: want %q got %q", k, wv, gv)
		}
	}
}

func TestTokenAuth_Get(t *testing.T) {
	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			return kvResponse(t, map[string]string{"JWT_SECRET": "the-secret"}), nil
		},
	}

	cfg := vault.Config{Addr: "http://vault.test:8200", Token: "mytoken"}
	p := vault.NewWithClient(cfg, client)

	val, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "JWT_SECRET")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "the-secret" {
		t.Errorf("want the-secret got %q", val)
	}
}

func TestTokenAuth_MissingKey(t *testing.T) {
	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			return kvResponse(t, map[string]string{"OTHER_KEY": "val"}), nil
		},
	}

	cfg := vault.Config{Addr: "http://vault.test:8200", Token: "mytoken"}
	p := vault.NewWithClient(cfg, client)

	_, err := p.Get(context.Background(), "foundryfabric", "dev", "hetzner", "ash", "MISSING_KEY")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("error should mention MISSING_KEY, got: %v", err)
	}
}

// ---- AppRole auth tests ----

func TestAppRoleAuth_LoginThenKVRead(t *testing.T) {
	loginCalled := false
	kvCalled := false

	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/approle/login"):
				loginCalled = true
				// Verify role_id and secret_id are sent.
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatalf("decode login body: %v", err)
				}
				if body["role_id"] != "myrole" {
					t.Errorf("role_id: want myrole got %q", body["role_id"])
				}
				return jsonResp(t, 200, map[string]interface{}{
					"auth": map[string]interface{}{
						"client_token": "approle-token",
					},
				}), nil

			case strings.Contains(r.URL.Path, "/secret/data/"):
				kvCalled = true
				if r.Header.Get("X-Vault-Token") != "approle-token" {
					t.Errorf("X-Vault-Token after AppRole: want approle-token got %q",
						r.Header.Get("X-Vault-Token"))
				}
				return kvResponse(t, map[string]string{"KEY": "val"}), nil

			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				return &http.Response{
					StatusCode: 404,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     make(http.Header),
				}, nil
			}
		},
	}

	cfg := vault.Config{
		Addr:     "http://vault.test:8200",
		RoleID:   "myrole",
		SecretID: "mysecret",
	}
	p := vault.NewWithClient(cfg, client)

	_, err := p.GetAll(context.Background(), "foundryfabric", "dev", "hetzner", "ash")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !loginCalled {
		t.Error("expected AppRole login to be called")
	}
	if !kvCalled {
		t.Error("expected KV read to be called")
	}
}

func TestAppRoleAuth_TokenCachedAfterLogin(t *testing.T) {
	loginCalls := 0

	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/approle/login") {
				loginCalls++
				return jsonResp(t, 200, map[string]interface{}{
					"auth": map[string]interface{}{"client_token": "approle-token"},
				}), nil
			}
			return kvResponse(t, map[string]string{"K": "v"}), nil
		},
	}

	cfg := vault.Config{Addr: "http://vault.test:8200", RoleID: "r", SecretID: "s"}
	p := vault.NewWithClient(cfg, client)

	for i := 0; i < 3; i++ {
		if _, err := p.GetAll(context.Background(), "app", "dev", "hetzner", "ash"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}

	if loginCalls != 1 {
		t.Errorf("expected 1 AppRole login, got %d", loginCalls)
	}
}

func TestKVRead_404ReturnsNotFoundError(t *testing.T) {
	client := &mockHTTPClient{
		fn: func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 404,
				Body:       io.NopCloser(strings.NewReader(`{"errors":[]}`)),
				Header:     make(http.Header),
			}, nil
		},
	}

	cfg := vault.Config{Addr: "http://vault.test:8200", Token: "tok"}
	p := vault.NewWithClient(cfg, client)

	_, err := p.GetAll(context.Background(), "app", "dev", "hetzner", "ash")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %v", err)
	}
}
