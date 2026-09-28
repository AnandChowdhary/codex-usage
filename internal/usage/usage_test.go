package usage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const fullPayload = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 28, "limit_window_seconds": 18000, "reset_after_seconds": 7980, "reset_at": 1790000000},
    "secondary_window": {"used_percent": 59.5, "limit_window_seconds": 604800, "reset_after_seconds": 250000, "reset_at": 1790250000}
  },
  "credits": {"has_credits": true, "unlimited": false, "balance": 4.2},
  "additional_rate_limits": [
    {"limit_name": "GPT-5.6-Codex-Spark", "metered_feature": "codex_spark", "rate_limit": null, "normal_model_slug": "x"}
  ],
  "rate_limit_reached_type": null,
  "spend_control": null,
  "account_id": "acct-1"
}`

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		headers := map[string]string{
			"Authorization":      "Bearer access",
			"ChatGPT-Account-Id": "acct-1",
			"X-OpenAI-Fedramp":   "true",
			"User-Agent":         "codex-usage/test",
		}
		for k, v := range headers {
			if got := r.Header.Get(k); got != v {
				t.Errorf("header %s = %q, want %q", k, got, v)
			}
		}
		io.WriteString(w, fullPayload)
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/backend-api/", srv.Client(), "codex-usage/test")
	got, err := c.Fetch(context.Background(), Credentials{AccessToken: "access", AccountID: "acct-1", FedRAMP: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanType != "plus" || got.RateLimit.PrimaryWindow.UsedPercent != 28 || got.RateLimit.SecondaryWindow.LeftPercent() != 40.5 {
		t.Fatalf("decoded %+v", got)
	}
	if string(got.Credits.Balance) != "4.2" || len(got.AdditionalRateLimits) != 1 || got.AdditionalRateLimits[0].RateLimit != nil {
		t.Fatalf("decoded credits/additional %+v %+v", got.Credits, got.AdditionalRateLimits)
	}
}

func TestFetchNulls(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-Id") != "" || r.Header.Get("X-OpenAI-Fedramp") != "" {
			t.Errorf("unexpected optional headers %v", r.Header)
		}
		io.WriteString(w, `{"plan_type":"free","rate_limit":null,"credits":null,"additional_rate_limits":null}`)
	}))
	defer srv.Close()
	got, err := NewClient(srv.URL, srv.Client(), "").Fetch(context.Background(), Credentials{AccessToken: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if got.RateLimit != nil || got.RateLimit.Windows() != nil || got.RateLimit.Blocked() {
		t.Fatalf("nil rate limit handled wrong: %+v", got.RateLimit)
	}
}

func TestFetchErrors(t *testing.T) {
	status := http.StatusUnauthorized
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, "<html>\n  service   unavailable\n</html>")
	}))
	defer srv.Close()
	c := NewClient(srv.URL, srv.Client(), "")

	if _, err := c.Fetch(context.Background(), Credentials{}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: err = %v", err)
	}
	status = http.StatusServiceUnavailable
	_, err := c.Fetch(context.Background(), Credentials{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 503: <html> service unavailable </html>") {
		t.Fatalf("503: err = %v", err)
	}
}

func TestWindow(t *testing.T) {
	labels := map[int64]string{18000: "5h", 604800: "weekly", 86400: "daily", 3600: "1h", 2592000: "30d", 90: "90s", 1800: "30m", 0: "limit"}
	for secs, want := range labels {
		if got := (&Window{LimitWindowSeconds: secs}).Label(); got != want {
			t.Errorf("Label(%d) = %q, want %q", secs, got, want)
		}
	}
	for used, want := range map[float64]float64{0: 100, 28: 72, 100: 0, 120: 0, -5: 100} {
		if got := (&Window{UsedPercent: used}).LeftPercent(); got != want {
			t.Errorf("LeftPercent(used %v) = %v, want %v", used, got, want)
		}
	}
	now := time.Unix(1000, 0)
	if got := (&Window{ResetAfterSeconds: 60}).ResetTime(now); !got.Equal(time.Unix(1060, 0)) {
		t.Errorf("ResetTime without reset_at = %v", got)
	}
	if got := (&Window{ResetAt: 5000, ResetAfterSeconds: 60}).ResetTime(now); !got.Equal(time.Unix(5000, 0)) {
		t.Errorf("ResetTime with reset_at = %v", got)
	}
}
