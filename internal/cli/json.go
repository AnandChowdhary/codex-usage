package cli

import (
	"encoding/json"
	"io"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

type jsonUsage struct {
	FetchedAt string        `json:"fetched_at"`
	Accounts  []jsonAccount `json:"accounts"`
}

type jsonAccount struct {
	Label                string            `json:"label"`
	Email                string            `json:"email,omitempty"`
	Plan                 string            `json:"plan,omitempty"`
	AccountID            string            `json:"chatgpt_account_id,omitempty"`
	RateLimit            *jsonRateLimit    `json:"rate_limit,omitempty"`
	CodeReviewRateLimit  *jsonRateLimit    `json:"code_review_rate_limit,omitempty"`
	AdditionalRateLimits []jsonAdditional  `json:"additional_rate_limits,omitempty"`
	Credits              *jsonCredits      `json:"credits,omitempty"`
	LimitReachedType     string            `json:"limit_reached_type,omitempty"`
	SpendLimitReached    bool              `json:"spend_limit_reached,omitempty"`
	ResetCredits         *jsonResetCredits `json:"rate_limit_reset_credits,omitempty"`
	NeedsRelogin         bool              `json:"needs_relogin,omitempty"`
	InCodex              bool              `json:"in_codex,omitempty"`
	Warning              string            `json:"warning,omitempty"`
	Error                string            `json:"error,omitempty"`
}

type jsonRateLimit struct {
	Allowed      bool         `json:"allowed"`
	LimitReached bool         `json:"limit_reached"`
	Windows      []jsonWindow `json:"windows"`
}

type jsonWindow struct {
	Name          string  `json:"name"`
	UsedPercent   float64 `json:"used_percent"`
	LeftPercent   float64 `json:"left_percent"`
	WindowSeconds int64   `json:"window_seconds"`
	ResetsAt      string  `json:"resets_at"`
}

type jsonAdditional struct {
	Name           string         `json:"name"`
	MeteredFeature string         `json:"metered_feature,omitempty"`
	RateLimit      *jsonRateLimit `json:"rate_limit,omitempty"`
}

// jsonResetCredits lists usage limit resets; credits holds the redeemable
// ones, soonest to expire first, when the account has any.
type jsonResetCredits struct {
	AvailableCount int64             `json:"available_count"`
	Credits        []jsonResetCredit `json:"credits,omitempty"`
}

type jsonResetCredit struct {
	ID          string `json:"id"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	ResetType   string `json:"reset_type,omitempty"`
	GrantedAt   string `json:"granted_at,omitempty"`
	ExpiresAt   string `json:"expires_at,omitempty"`
}

type jsonCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

func (e *env) usageJSON(results []result) jsonUsage {
	now := e.Now()
	out := jsonUsage{FetchedAt: formatRFC3339(now), Accounts: []jsonAccount{}}
	inCodex := e.codexKey()
	for _, r := range results {
		acct := jsonAccount{
			Label:     r.account.Label,
			Email:     r.account.Email,
			Plan:      r.account.PlanType,
			AccountID: r.account.AccountID,
			InCodex:   r.account.Key() == inCodex,
		}
		if _, ok := r.err.(*reloginError); ok {
			acct.NeedsRelogin = true
		}
		if r.err != nil {
			acct.Error = r.err.Error()
		}
		if r.warning != nil {
			acct.Warning = r.warning.Error()
		}
		if u := r.usage; u != nil {
			if u.PlanType != "" {
				acct.Plan = u.PlanType
			}
			acct.RateLimit = rateLimitJSON(u.RateLimit, now)
			acct.CodeReviewRateLimit = rateLimitJSON(u.CodeReviewRateLimit, now)
			acct.SpendLimitReached = u.SpendControl != nil && u.SpendControl.Reached
			for _, extra := range u.AdditionalRateLimits {
				acct.AdditionalRateLimits = append(acct.AdditionalRateLimits, jsonAdditional{
					Name:           extra.LimitName,
					MeteredFeature: extra.MeteredFeature,
					RateLimit:      rateLimitJSON(extra.RateLimit, now),
				})
			}
			if c := u.Credits; c != nil {
				acct.Credits = &jsonCredits{HasCredits: c.HasCredits, Unlimited: c.Unlimited, Balance: string(c.Balance)}
			}
			if u.RateLimitReachedType != nil {
				acct.LimitReachedType = u.RateLimitReachedType.Type
			}
			if u.RateLimitResetCredits != nil || r.resets != nil {
				resets := &jsonResetCredits{AvailableCount: resetCount(r)}
				for _, c := range r.resets.Available() {
					resets.Credits = append(resets.Credits, jsonResetCredit{
						ID:          c.ID,
						Title:       c.Title,
						Description: c.Description,
						ResetType:   c.ResetType,
						GrantedAt:   c.GrantedAt,
						ExpiresAt:   c.ExpiresAt,
					})
				}
				acct.ResetCredits = resets
			}
		}
		out.Accounts = append(out.Accounts, acct)
	}
	return out
}

func rateLimitJSON(r *usage.RateLimit, now time.Time) *jsonRateLimit {
	if r == nil {
		return nil
	}
	out := &jsonRateLimit{Allowed: r.Allowed, LimitReached: r.LimitReached, Windows: []jsonWindow{}}
	for _, w := range r.Windows() {
		out.Windows = append(out.Windows, jsonWindow{
			Name:          w.Label(),
			UsedPercent:   w.UsedPercent,
			LeftPercent:   w.LeftPercent(),
			WindowSeconds: w.LimitWindowSeconds,
			ResetsAt:      formatRFC3339(w.ResetTime(now)),
		})
	}
	return out
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
