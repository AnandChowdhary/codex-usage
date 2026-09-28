package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const oauthRevokePath = "/oauth/revoke"

// RefreshError is a failed token refresh. Permanent errors mean the refresh
// token is no longer usable and the account has to sign in again.
type RefreshError struct {
	Status    int
	Code      string
	Message   string
	Permanent bool
}

func (e *RefreshError) Error() string {
	if e.Permanent {
		switch strings.ToLower(e.Code) {
		case "refresh_token_expired":
			return "sign-in expired"
		case "refresh_token_reused":
			return "sign-in was used elsewhere and is no longer valid"
		case "refresh_token_invalidated":
			return "sign-in was revoked"
		}
		return "sign-in is no longer valid"
	}
	return fmt.Sprintf("refreshing sign-in failed: HTTP %d: %s", e.Status, e.Message)
}

// Refresh exchanges a refresh token for new tokens. Any returned field may be
// empty, meaning the stored value stays current. Refresh tokens rotate, so a
// returned RefreshToken must be persisted before the old one is used again.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (*Tokens, error) {
	status, body, err := c.postJSON(ctx, c.Issuer+oauthTokenPath, map[string]string{
		"grant_type":    "refresh_token",
		"client_id":     c.ClientID,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, fmt.Errorf("refreshing sign-in: %w", err)
	}
	if status >= 200 && status <= 299 {
		var tokens Tokens
		if err := json.Unmarshal(body, &tokens); err != nil {
			return nil, fmt.Errorf("decoding refreshed tokens: %w", err)
		}
		return &tokens, nil
	}

	code, message := errorDetail(body)
	if message == "" {
		message = http.StatusText(status)
	}
	lower := strings.ToLower(code)
	permanent := status == http.StatusUnauthorized ||
		lower == "refresh_token_expired" ||
		lower == "refresh_token_reused" ||
		lower == "refresh_token_invalidated" ||
		(status == http.StatusBadRequest && lower == "invalid_grant")
	return nil, &RefreshError{Status: status, Code: code, Message: message, Permanent: permanent}
}

// Revoke invalidates a refresh token on the server.
func (c *Client) Revoke(ctx context.Context, refreshToken string) error {
	status, body, err := c.postJSON(ctx, c.Issuer+oauthRevokePath, map[string]string{
		"token":           refreshToken,
		"token_type_hint": "refresh_token",
		"client_id":       c.ClientID,
	})
	if err != nil {
		return fmt.Errorf("revoking sign-in: %w", err)
	}
	if status < 200 || status > 299 {
		return statusError("revoking sign-in", status, body)
	}
	return nil
}
