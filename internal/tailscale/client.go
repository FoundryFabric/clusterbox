// Package tailscale provides a client for generating ephemeral Tailscale auth keys
// via the Tailscale OAuth API. Auth key values are never logged.
package tailscale

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	oauthTokenURL    = "https://api.tailscale.com/api/v2/oauth/token"
	authKeysURL      = "https://api.tailscale.com/api/v2/tailnet/-/keys"
	keyExpirySeconds = 300
)

// HTTPClient is the interface used for HTTP requests, allowing injection in tests.
type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client is a Tailscale API client.
type Client struct {
	http HTTPClient
}

// New returns a Client using the provided HTTPClient. If httpClient is nil,
// http.DefaultClient is used.
func New(httpClient HTTPClient) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{http: httpClient}
}

// oauthTokenResponse is the response from the Tailscale OAuth token endpoint.
type oauthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// authKeyCapabilities describes the device-creation capabilities for a new auth key.
type authKeyCapabilities struct {
	Devices struct {
		Create struct {
			Reusable      bool     `json:"reusable"`
			Ephemeral     bool     `json:"ephemeral"`
			Preauthorized bool     `json:"preauthorized"`
			Tags          []string `json:"tags"`
		} `json:"create"`
	} `json:"devices"`
}

// authKeyRequest is the request body for creating a Tailscale auth key.
type authKeyRequest struct {
	Capabilities  authKeyCapabilities `json:"capabilities"`
	ExpirySeconds int                 `json:"expirySeconds"`
}

// authKeyResponse is the response from the Tailscale auth key creation endpoint.
type authKeyResponse struct {
	Key string `json:"key"`
}

// GenerateAuthKey exchanges the provided OAuth client credentials for a fresh
// ephemeral Tailscale auth key. The key is suitable for single-use VM
// first-boot activation. The key value is never written to any log.
// tags must be non-empty; Tailscale requires at least one ACL tag when the key
// is generated via an OAuth client.
func GenerateAuthKey(ctx context.Context, clientID, clientSecret string, tags []string) (string, error) {
	return New(nil).GenerateAuthKey(ctx, clientID, clientSecret, tags)
}

// GenerateAuthKey exchanges OAuth client credentials for a fresh ephemeral
// Tailscale auth key using the client's configured HTTPClient.
func (c *Client) GenerateAuthKey(ctx context.Context, clientID, clientSecret string, tags []string) (string, error) {
	token, err := c.fetchOAuthToken(ctx, clientID, clientSecret)
	if err != nil {
		return "", fmt.Errorf("tailscale: fetch oauth token: %w", err)
	}

	key, err := c.createAuthKey(ctx, token, tags)
	if err != nil {
		return "", fmt.Errorf("tailscale: create auth key: %w", err)
	}

	return key, nil
}

// fetchOAuthToken obtains a bearer token from the Tailscale OAuth endpoint
// using the client_credentials grant.
func (c *Client) fetchOAuthToken(ctx context.Context, clientID, clientSecret string) (string, error) {
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, oauthTokenURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth token endpoint returned HTTP %d: %s",
			resp.StatusCode, sanitize(string(body)))
	}

	var tok oauthTokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("oauth token response missing access_token")
	}

	return tok.AccessToken, nil
}

// createAuthKey creates a new ephemeral, non-reusable, pre-authorised auth key
// using the provided bearer token.
func (c *Client) createAuthKey(ctx context.Context, bearerToken string, tags []string) (string, error) {
	caps := authKeyCapabilities{}
	caps.Devices.Create.Reusable = false
	caps.Devices.Create.Ephemeral = true
	caps.Devices.Create.Preauthorized = true
	caps.Devices.Create.Tags = tags

	reqBody := authKeyRequest{
		Capabilities:  caps,
		ExpirySeconds: keyExpirySeconds,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authKeysURL,
		bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bearerToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("auth key endpoint returned HTTP %d: %s",
			resp.StatusCode, sanitize(string(respBody)))
	}

	var keyResp authKeyResponse
	if err := json.Unmarshal(respBody, &keyResp); err != nil {
		return "", fmt.Errorf("decode auth key response: %w", err)
	}
	if keyResp.Key == "" {
		return "", fmt.Errorf("auth key response missing key field")
	}

	return keyResp.Key, nil
}

// sanitize truncates long error bodies to avoid leaking sensitive data in errors.
func sanitize(s string) string {
	const maxLen = 200
	if len(s) > maxLen {
		return s[:maxLen] + "..."
	}
	return s
}
