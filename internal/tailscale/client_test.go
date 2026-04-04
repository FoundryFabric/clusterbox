package tailscale_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"

	"github.com/foundryfabric/clusterbox/internal/tailscale"
)

// roundTripFunc is a helper that adapts a function to the http.RoundTripper interface.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// mockHTTPClient returns an HTTPClient backed by two response factories:
// one for the OAuth token request and one for the auth-key creation request.
func mockHTTPClient(tokenResp, keyResp *http.Response) *http.Client {
	callCount := 0
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			if callCount == 1 {
				return tokenResp, nil
			}
			return keyResp, nil
		}),
	}
}

func oauthTokenResponse(token string) *http.Response {
	body, _ := json.Marshal(map[string]string{
		"access_token": token,
		"token_type":   "Bearer",
	})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func authKeyResponse(key string) *http.Response {
	body, _ := json.Marshal(map[string]string{"key": key})
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
	}
}

func errorResponse(statusCode int, message string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(message)),
		Header:     make(http.Header),
	}
}

// TestGenerateAuthKey_Success verifies that a happy-path call hits the OAuth
// endpoint and returns the key string.
func TestGenerateAuthKey_Success(t *testing.T) {
	const wantKey = "tskey-auth-abc123-XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

	httpClient := mockHTTPClient(
		oauthTokenResponse("test-access-token"),
		authKeyResponse(wantKey),
	)

	client := tailscale.New(httpClient)
	got, err := client.GenerateAuthKey(context.Background(), "client-id", "client-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != wantKey {
		t.Errorf("key = %q; want %q", got, wantKey)
	}
}

// TestGenerateAuthKey_OAuthEndpointError verifies that a non-200 from the
// OAuth token endpoint returns a descriptive error without proceeding to key
// creation.
func TestGenerateAuthKey_OAuthEndpointError(t *testing.T) {
	httpClient := mockHTTPClient(
		errorResponse(http.StatusUnauthorized, `{"error":"invalid_client"}`),
		nil, // should never be called
	)

	client := tailscale.New(httpClient)
	_, err := client.GenerateAuthKey(context.Background(), "bad-id", "bad-secret")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q should mention HTTP status 401", err.Error())
	}
}

// TestGenerateAuthKey_AuthKeyEndpointError verifies that a non-200 from the
// auth-key creation endpoint returns a descriptive error.
func TestGenerateAuthKey_AuthKeyEndpointError(t *testing.T) {
	httpClient := mockHTTPClient(
		oauthTokenResponse("valid-token"),
		errorResponse(http.StatusForbidden, `{"message":"insufficient permissions"}`),
	)

	client := tailscale.New(httpClient)
	_, err := client.GenerateAuthKey(context.Background(), "client-id", "client-secret")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error %q should mention HTTP status 403", err.Error())
	}
}

// TestGenerateAuthKey_KeyNotInLogs verifies that the returned auth key value
// never appears in log output.
func TestGenerateAuthKey_KeyNotInLogs(t *testing.T) {
	const secretKey = "tskey-auth-SUPERSECRET-XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

	// Redirect the default logger to a buffer so we can inspect its output.
	var logBuf bytes.Buffer
	origOutput := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(origOutput) })

	httpClient := mockHTTPClient(
		oauthTokenResponse("some-access-token"),
		authKeyResponse(secretKey),
	)

	client := tailscale.New(httpClient)
	got, err := client.GenerateAuthKey(context.Background(), "client-id", "client-secret")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != secretKey {
		t.Fatalf("key = %q; want %q", got, secretKey)
	}

	logged := logBuf.String()
	if strings.Contains(logged, secretKey) {
		t.Errorf("log output contains the secret key value — must not log key material")
	}
}

// TestGenerateAuthKey_RequestShape verifies that the OAuth request uses
// client_credentials grant type and the auth-key request sets ephemeral=true
// and reusable=false.
func TestGenerateAuthKey_RequestShape(t *testing.T) {
	const wantKey = "tskey-auth-shape-XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"

	var oauthReq, keyReq *http.Request
	callCount := 0

	httpClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			callCount++
			cloned := r.Clone(r.Context())
			// Read and re-set the body so we can inspect it.
			if r.Body != nil {
				bodyBytes, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				cloned.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			}
			if callCount == 1 {
				oauthReq = cloned
				return oauthTokenResponse("token"), nil
			}
			keyReq = cloned
			return authKeyResponse(wantKey), nil
		}),
	}

	client := tailscale.New(httpClient)
	if _, err := client.GenerateAuthKey(context.Background(), "cid", "csecret"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify OAuth request.
	if oauthReq == nil {
		t.Fatal("oauth request was never made")
	}
	oauthBody, _ := io.ReadAll(oauthReq.Body)
	if !strings.Contains(string(oauthBody), "grant_type=client_credentials") {
		t.Errorf("oauth body %q missing grant_type=client_credentials", string(oauthBody))
	}

	// Verify auth-key request body.
	if keyReq == nil {
		t.Fatal("auth-key request was never made")
	}
	keyBody, _ := io.ReadAll(keyReq.Body)
	var keyPayload struct {
		Capabilities struct {
			Devices struct {
				Create struct {
					Reusable  bool `json:"reusable"`
					Ephemeral bool `json:"ephemeral"`
				} `json:"create"`
			} `json:"devices"`
		} `json:"capabilities"`
		ExpirySeconds int `json:"expirySeconds"`
	}
	if err := json.Unmarshal(keyBody, &keyPayload); err != nil {
		t.Fatalf("decode key request body: %v", err)
	}
	if keyPayload.Capabilities.Devices.Create.Reusable {
		t.Error("key request: reusable must be false")
	}
	if !keyPayload.Capabilities.Devices.Create.Ephemeral {
		t.Error("key request: ephemeral must be true")
	}
	if keyPayload.ExpirySeconds != 300 {
		t.Errorf("expirySeconds = %d; want 300", keyPayload.ExpirySeconds)
	}
}
