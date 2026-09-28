// Package auth implements the ChatGPT sign-in used by the Codex CLI: device-code
// login, token refresh and revocation against auth.openai.com.
package auth

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
	// DefaultIssuer is the OpenAI auth server.
	DefaultIssuer = "https://auth.openai.com"
	// ClientID is the public OAuth client of the Codex CLI.
	ClientID = "app_EMoamEEZ73f0CkXaXp7hrann"

	maxBodyBytes = 1 << 20
)

// Tokens is the credential set issued by the auth server.
type Tokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

// Client talks to the auth server.
type Client struct {
	Issuer    string
	ClientID  string
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a Client for issuer, falling back to DefaultIssuer.
func NewClient(issuer string, httpClient *http.Client, userAgent string) *Client {
	if issuer == "" {
		issuer = DefaultIssuer
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		Issuer:    strings.TrimRight(issuer, "/"),
		ClientID:  ClientID,
		HTTP:      httpClient,
		UserAgent: userAgent,
	}
}

func (c *Client) postJSON(ctx context.Context, endpoint string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, err
	}
	return c.post(ctx, endpoint, "application/json", bytes.NewReader(body))
}

func (c *Client) postForm(ctx context.Context, endpoint string, form url.Values) (int, []byte, error) {
	return c.post(ctx, endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

func (c *Client) post(ctx context.Context, endpoint, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

// errorDetail extracts an OAuth error code and message from a response body,
// accepting both {"error": "code"} and {"error": {"code", "message"}} shapes.
func errorDetail(body []byte) (code, message string) {
	var parsed map[string]any
	if json.Unmarshal(body, &parsed) != nil {
		return "", strings.TrimSpace(string(body))
	}
	switch e := parsed["error"].(type) {
	case string:
		code = e
	case map[string]any:
		code, _ = e["code"].(string)
		message, _ = e["message"].(string)
	}
	if code == "" {
		code, _ = parsed["code"].(string)
	}
	if desc, ok := parsed["error_description"].(string); ok && desc != "" {
		message = desc
	}
	if message == "" {
		message = code
	}
	return code, message
}

func statusError(action string, status int, body []byte) error {
	_, message := errorDetail(body)
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("%s failed: HTTP %d: %s", action, status, message)
}
