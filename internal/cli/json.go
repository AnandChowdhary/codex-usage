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
	Label                string           `json:"label"`
	Email                string           `json:"email,omitempty"`
	Plan                 string           `json:"plan,omitempty"`
	AccountID            string           `json:"chatgpt_account_id,omitempty"`
	RateLimit            *jsonRateLimit   `json:"rate_limit,omitempty"`
	AdditionalRateLimits []jsonAdditional `json:"additional_rate_limits,omitempty"`
	Credits              *jsonCredits     `json:"credits,omitempty"`
	LimitReachedType     string           `json:"limit_reached_type,omitempty"`
	NeedsRelogin         bool             `json:"needs_relogin,omitempty"`
	Warning              string           `json:"warning,omitempty"`
	Error                string           `json:"error,omitempty"`
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

type jsonCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

func (e *env) usageJSON(results []result) jsonUsage {
	now := e.Now()
	out := jsonUsage{FetchedAt: formatRFC3339(now), Accounts: []jsonAccount{}}
	for _, r := range results {
		acct := jsonAccount{
			Label:     r.account.Label,
			Email:     r.account.Email,
			Plan:      r.account.PlanType,
			AccountID: r.account.AccountID,
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
