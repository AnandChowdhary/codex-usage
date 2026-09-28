package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	deviceCodeLifetime    = 15 * time.Minute
	defaultPollInterval   = 5 * time.Second
	deviceCallbackPath    = "/deviceauth/callback"
	deviceVerifyPath      = "/codex/device"
	deviceUserCodePath    = "/api/accounts/deviceauth/usercode"
	deviceTokenPath       = "/api/accounts/deviceauth/token"
	oauthTokenPath        = "/oauth/token"
	deviceDisabledMessage = "device code sign-in is not enabled; allow it in ChatGPT's security settings (or ask your workspace admin), then try again"
)

var (
	// ErrDeviceAuthDisabled is returned when the server does not offer device-code login.
	ErrDeviceAuthDisabled = errors.New(deviceDisabledMessage)
	// ErrDeviceCodeExpired is returned when the code was not approved in time.
	ErrDeviceCodeExpired = errors.New("the sign-in code expired before it was approved; run login again")
)

// DeviceCode is a pending device-code login.
type DeviceCode struct {
	VerificationURL string
	UserCode        string
	ExpiresAt       time.Time

	deviceAuthID string
	interval     time.Duration
}

// seconds decodes a number of seconds sent either as a JSON number or string.
type seconds int64

func (s *seconds) UnmarshalJSON(data []byte) error {
	text := strings.Trim(string(data), `"`)
	if text == "" || text == "null" {
		*s = 0
		return nil
	}
	n, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		return fmt.Errorf("invalid seconds value %s", data)
	}
	*s = seconds(n)
	return nil
}

// RequestDeviceCode starts a device-code login.
func (c *Client) RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	status, body, err := c.postJSON(ctx, c.Issuer+deviceUserCodePath, map[string]string{
		"client_id": c.ClientID,
	})
	if err != nil {
		return nil, fmt.Errorf("requesting sign-in code: %w", err)
	}
	if status == http.StatusNotFound {
		return nil, ErrDeviceAuthDisabled
	}
	if status < 200 || status > 299 {
		return nil, statusError("requesting sign-in code", status, body)
	}

	var resp struct {
		DeviceAuthID string  `json:"device_auth_id"`
		UserCode     string  `json:"user_code"`
		UserCodeAlt  string  `json:"usercode"`
		Interval     seconds `json:"interval"`
		ExpiresAt    string  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("decoding sign-in code: %w", err)
	}
	code := resp.UserCode
	if code == "" {
		code = resp.UserCodeAlt
	}
	if resp.DeviceAuthID == "" || code == "" {
		return nil, errors.New("sign-in code response is missing device_auth_id or user_code")
	}
	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = defaultPollInterval
	}
	// A missing or unparseable expiry falls back to the documented 15 minutes.
	expiresAt, _ := time.Parse(time.RFC3339Nano, resp.ExpiresAt)
	return &DeviceCode{
		VerificationURL: c.Issuer + deviceVerifyPath,
		UserCode:        code,
		ExpiresAt:       expiresAt,
		deviceAuthID:    resp.DeviceAuthID,
		interval:        interval,
	}, nil
}

// CompleteDeviceLogin waits for the user to approve the code, then exchanges
// the resulting authorization code for tokens.
func (c *Client) CompleteDeviceLogin(ctx context.Context, dc *DeviceCode) (*Tokens, error) {
	grant, err := c.pollDeviceCode(ctx, dc)
	if err != nil {
		return nil, err
	}
	return c.exchangeCode(ctx, grant)
}

type codeGrant struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func (c *Client) pollDeviceCode(ctx context.Context, dc *DeviceCode) (*codeGrant, error) {
	deadline := time.Now().Add(deviceCodeLifetime)
	if !dc.ExpiresAt.IsZero() && dc.ExpiresAt.Before(deadline) {
		deadline = dc.ExpiresAt
	}
	payload := map[string]string{
		"device_auth_id": dc.deviceAuthID,
		"user_code":      dc.UserCode,
	}
	for {
		status, body, err := c.postJSON(ctx, c.Issuer+deviceTokenPath, payload)
		switch {
		case ctx.Err() != nil:
			return nil, ctx.Err()
		case err != nil:
			// Network blips shouldn't abandon a login the user may be approving.
		case status >= 200 && status <= 299:
			var grant codeGrant
			if err := json.Unmarshal(body, &grant); err != nil {
				return nil, fmt.Errorf("decoding approved sign-in: %w", err)
			}
			if grant.AuthorizationCode == "" || grant.CodeVerifier == "" {
				return nil, errors.New("approved sign-in is missing authorization_code or code_verifier")
			}
			return &grant, nil
		case status == http.StatusForbidden || status == http.StatusNotFound:
			// Not approved yet.
		case status == http.StatusTooManyRequests || status >= 500:
			// Transient; keep polling.
		default:
			return nil, statusError("waiting for sign-in approval", status, body)
		}

		wait := dc.interval
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrDeviceCodeExpired
		}
		if remaining < wait {
			wait = remaining
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// exchangeCode trades the authorization code for tokens. It is never retried:
// the code is single-use and a timed-out request may already have consumed it.
func (c *Client) exchangeCode(ctx context.Context, grant *codeGrant) (*Tokens, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {c.ClientID},
		"code":          {grant.AuthorizationCode},
		"redirect_uri":  {c.Issuer + deviceCallbackPath},
		"code_verifier": {grant.CodeVerifier},
	}
	status, body, err := c.postForm(ctx, c.Issuer+oauthTokenPath, form)
	if err != nil {
		return nil, fmt.Errorf("exchanging sign-in code: %w", err)
	}
	if status < 200 || status > 299 {
		return nil, statusError("exchanging sign-in code", status, body)
	}
	var tokens Tokens
	if err := json.Unmarshal(body, &tokens); err != nil {
		return nil, fmt.Errorf("decoding tokens: %w", err)
	}
	if tokens.IDToken == "" || tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return nil, errors.New("token response is missing id_token, access_token or refresh_token")
	}
	return &tokens, nil
}
