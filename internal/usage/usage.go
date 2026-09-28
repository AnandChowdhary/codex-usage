// Package usage reads Codex rate-limit status from the ChatGPT backend.
package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is the ChatGPT backend used by the Codex CLI.
const DefaultBaseURL = "https://chatgpt.com/backend-api"

const maxBodyBytes = 1 << 20

// ErrUnauthorized means the access token was rejected.
var ErrUnauthorized = errors.New("access token was rejected")

// Client fetches usage from the ChatGPT backend.
type Client struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
}

// NewClient returns a Client for baseURL, falling back to DefaultBaseURL.
func NewClient(baseURL string, httpClient *http.Client, userAgent string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: httpClient, UserAgent: userAgent}
}

// Credentials authorize a usage request for one workspace.
type Credentials struct {
	AccessToken string
	AccountID   string
	FedRAMP     bool
}

// Window is one rate-limit window, e.g. the rolling 5 hours or the week.
type Window struct {
	UsedPercent        float64 `json:"used_percent"`
	LimitWindowSeconds int64   `json:"limit_window_seconds"`
	ResetAfterSeconds  int64   `json:"reset_after_seconds"`
	ResetAt            int64   `json:"reset_at"`
}

// RateLimit is the state of a limit and its windows.
type RateLimit struct {
	Allowed         bool    `json:"allowed"`
	LimitReached    bool    `json:"limit_reached"`
	PrimaryWindow   *Window `json:"primary_window"`
	SecondaryWindow *Window `json:"secondary_window"`
}

// Credits is the purchased-credit balance.
type Credits struct {
	HasCredits bool       `json:"has_credits"`
	Unlimited  bool       `json:"unlimited"`
	Balance    flexString `json:"balance"`
}

// AdditionalRateLimit is a separately metered limit, e.g. for one model.
type AdditionalRateLimit struct {
	LimitName      string     `json:"limit_name"`
	MeteredFeature string     `json:"metered_feature"`
	RateLimit      *RateLimit `json:"rate_limit"`
}

// Response is the subset of /wham/usage this tool reads. Every nested value
// may be null or missing.
type Response struct {
	PlanType             string                `json:"plan_type"`
	RateLimit            *RateLimit            `json:"rate_limit"`
	CodeReviewRateLimit  *RateLimit            `json:"code_review_rate_limit"`
	AdditionalRateLimits []AdditionalRateLimit `json:"additional_rate_limits"`
	Credits              *Credits              `json:"credits"`
	SpendControl         *struct {
		Reached bool `json:"reached"`
	} `json:"spend_control"`
	RateLimitReachedType *struct {
		Type string `json:"type"`
	} `json:"rate_limit_reached_type"`
	RateLimitResetCredits *struct {
		AvailableCount int64 `json:"available_count"`
	} `json:"rate_limit_reset_credits"`
}

// AvailableResets is the number of usage limit resets the account can redeem.
func (r *Response) AvailableResets() int64 {
	if r == nil || r.RateLimitResetCredits == nil {
		return 0
	}
	return max(r.RateLimitResetCredits.AvailableCount, 0)
}

// ResetCredits is the list of usage limit resets from
// /wham/rate-limit-reset-credits. Redeeming one clears the current limits.
type ResetCredits struct {
	Credits          []ResetCredit `json:"credits"`
	AvailableCount   int64         `json:"available_count"`
	TotalEarnedCount int64         `json:"total_earned_count"`
}

// ResetCredit is one earned usage limit reset.
type ResetCredit struct {
	ID          string `json:"id"`
	ResetType   string `json:"reset_type"`
	Status      string `json:"status"` // available, redeeming or redeemed
	GrantedAt   string `json:"granted_at"`
	ExpiresAt   string `json:"expires_at"` // RFC 3339; empty if it never expires
	Title       string `json:"title"`
	Description string `json:"description"`
}

// Expiry is when the credit expires, if it does.
func (c ResetCredit) Expiry() (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, c.ExpiresAt)
	return t, err == nil
}

// Available returns the redeemable credits, soonest to expire first.
func (r *ResetCredits) Available() []ResetCredit {
	if r == nil {
		return nil
	}
	var out []ResetCredit
	for _, c := range r.Credits {
		if c.Status == "available" {
			out = append(out, c)
		}
	}
	slices.SortStableFunc(out, func(a, b ResetCredit) int {
		ta, okA := a.Expiry()
		tb, okB := b.Expiry()
		switch {
		case okA && okB:
			return ta.Compare(tb)
		case okA:
			return -1
		case okB:
			return 1
		}
		return 0
	})
	return out
}

// Fetch returns the current usage for one workspace.
func (c *Client) Fetch(ctx context.Context, creds Credentials) (*Response, error) {
	var out Response
	if err := c.get(ctx, "/wham/usage", creds, "usage", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// FetchResetCredits lists the usage limit resets a workspace has earned.
func (c *Client) FetchResetCredits(ctx context.Context, creds Credentials) (*ResetCredits, error) {
	var out ResetCredits
	if err := c.get(ctx, "/wham/rate-limit-reset-credits", creds, "usage limit resets", &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) get(ctx context.Context, path string, creds Credentials, what string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+creds.AccessToken)
	req.Header.Set("Accept", "application/json")
	if creds.AccountID != "" {
		req.Header.Set("ChatGPT-Account-Id", creds.AccountID)
	}
	if creds.FedRAMP {
		req.Header.Set("X-OpenAI-Fedramp", "true")
	}
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", what, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return fmt.Errorf("fetching %s: %w", what, err)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("fetching %s: HTTP %d: %s", what, resp.StatusCode, snippet(body))
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding %s: %w", what, err)
	}
	return nil
}

// LeftPercent is the share of the window still available, clamped to 0–100.
func (w *Window) LeftPercent() float64 {
	return min(max(100-w.UsedPercent, 0), 100)
}

// ResetTime is when the window resets.
func (w *Window) ResetTime(now time.Time) time.Time {
	if w.ResetAt > 0 {
		return time.Unix(w.ResetAt, 0)
	}
	return now.Add(time.Duration(w.ResetAfterSeconds) * time.Second)
}

// Label names the window by its length: "5h", "weekly", "daily", or a duration.
func (w *Window) Label() string {
	s := w.LimitWindowSeconds
	switch {
	case s == 7*24*3600:
		return "weekly"
	case s == 24*3600:
		return "daily"
	case s <= 0:
		return "limit"
	case s%86400 == 0:
		return strconv.FormatInt(s/86400, 10) + "d"
	case s%3600 == 0:
		return strconv.FormatInt(s/3600, 10) + "h"
	case s%60 == 0:
		return strconv.FormatInt(s/60, 10) + "m"
	default:
		return strconv.FormatInt(s, 10) + "s"
	}
}

// Windows returns the non-nil windows of a limit, primary first.
func (r *RateLimit) Windows() []*Window {
	if r == nil {
		return nil
	}
	var out []*Window
	for _, w := range []*Window{r.PrimaryWindow, r.SecondaryWindow} {
		if w != nil {
			out = append(out, w)
		}
	}
	return out
}

// Blocked reports whether the limit currently stops usage.
func (r *RateLimit) Blocked() bool {
	return r != nil && (r.LimitReached || !r.Allowed)
}

// flexString decodes a JSON string or number as text.
type flexString string

func (s *flexString) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*s = ""
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*s = flexString(text)
		return nil
	}
	var num json.Number
	if err := json.Unmarshal(data, &num); err != nil {
		return fmt.Errorf("invalid balance %s", data)
	}
	*s = flexString(num.String())
	return nil
}

func snippet(body []byte) string {
	text := strings.Join(strings.Fields(string(body)), " ")
	if runes := []rune(text); len(runes) > 200 {
		text = string(runes[:200]) + "…"
	}
	if text == "" {
		text = "empty response"
	}
	return text
}
