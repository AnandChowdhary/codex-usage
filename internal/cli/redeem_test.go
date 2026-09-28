package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

func init() { redeemRetryDelay = time.Millisecond }

// redeemHarness has one account, "work", at its 5h limit with two resets.
func redeemHarness(t *testing.T) (*harness, string) {
	h := newHarness(t)
	access := accessToken(t, "work", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", access, "rt"))
	h.backend.usage[access] = usageBody("plus", 100, 60, `, "rate_limit_reset_credits": {"available_count": 2}`)
	h.backend.resets[access] = `{"credits": [
	  {"id": "later", "status": "available", "granted_at": "2026-09-01T00:00:00Z", "expires_at": "2026-10-10T00:00:00Z"},
	  {"id": "soon", "status": "available", "granted_at": "2026-09-01T00:00:00Z", "expires_at": "2026-09-29T09:00:00Z"}
	], "available_count": 2}`
	h.backend.onRedeem = func() {
		h.backend.usage[access] = usageBody("plus", 0, 0, `, "rate_limit_reset_credits": {"available_count": 1}`)
	}
	return h, access
}

func TestRedeem(t *testing.T) {
	h, _ := redeemHarness(t)
	h.backend.redeemReply = []refreshReply{{200, `{"code":"reset","windows_reset":2}`}}
	h.stdin = "y\n"

	out, errOut, code := h.run("redeem", "work")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, out %q", code, errOut, out)
	}
	want := strings.Join([]string{
		"work@acme.com (plus)",
		"  5h        0% left, resets in 2h13m",
		"  weekly   40% left, resets Thu 09:00",
		"  2 usage limit resets available (first expires in 21h00m)",
		"",
		"Use a usage limit reset on work? This clears its current usage limits and can't be undone. [y/N] ✓ Usage limits reset for work (2 windows).",
		"  5h      100% left, resets in 2h13m",
		"  weekly  100% left, resets Thu 09:00",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}
	if len(h.backend.redeemCalls) != 1 {
		t.Fatalf("redeem calls %v", h.backend.redeemCalls)
	}
	body := h.backend.redeemCalls[0]
	if body["credit_id"] != "soon" {
		t.Errorf("redeemed %q, want the reset that expires first", body["credit_id"])
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(body["redeem_request_id"]) {
		t.Errorf("redeem_request_id %q is not a UUID v4", body["redeem_request_id"])
	}
}

func TestRedeemNeedsConfirmation(t *testing.T) {
	for _, stdin := range []string{"n\n", "\n", "", "yes please\n"} {
		h, _ := redeemHarness(t)
		h.stdin = stdin
		out, _, code := h.run("redeem", "work")
		if code != 0 || !strings.HasSuffix(out, "Cancelled; nothing was redeemed.\n") {
			t.Errorf("stdin %q: exit %d, output %q", stdin, code, out)
		}
		if len(h.backend.redeemCalls) != 0 {
			t.Errorf("stdin %q: redeemed without confirmation", stdin)
		}
	}
}

func TestRedeemYesAndCredit(t *testing.T) {
	h, _ := redeemHarness(t)
	if _, errOut, code := h.run("redeem", "work@acme.com", "--yes", "--credit", "later"); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(h.backend.redeemCalls) != 1 || h.backend.redeemCalls[0]["credit_id"] != "later" {
		t.Fatalf("redeem calls %v", h.backend.redeemCalls)
	}

	h, _ = redeemHarness(t)
	_, errOut, code := h.run("redeem", "work", "--yes", "--credit", "nope")
	if code != 1 || !strings.Contains(errOut, `no available reset has ID "nope"`) || len(h.backend.redeemCalls) != 0 {
		t.Fatalf("unknown credit: exit %d, %q, calls %v", code, errOut, h.backend.redeemCalls)
	}
}

func TestRedeemRetriesWithSameRequestID(t *testing.T) {
	h, _ := redeemHarness(t)
	h.backend.redeemReply = []refreshReply{{503, "busy"}, {502, "bad gateway"}, {200, `{"code":"reset","windows_reset":1}`}}
	out, errOut, code := h.run("redeem", "work", "--yes")
	if code != 0 || !strings.Contains(out, "Usage limits reset for work (1 window).") {
		t.Fatalf("exit %d, out %q, err %q", code, out, errOut)
	}
	calls := h.backend.redeemCalls
	if len(calls) != 3 || calls[0]["redeem_request_id"] != calls[1]["redeem_request_id"] || calls[1]["redeem_request_id"] != calls[2]["redeem_request_id"] {
		t.Fatalf("retries must reuse the request ID: %v", calls)
	}
}

func TestRedeemGivesUpWithWarning(t *testing.T) {
	h, _ := redeemHarness(t)
	h.backend.redeemReply = []refreshReply{{503, "busy"}}
	_, errOut, code := h.run("redeem", "work", "--yes")
	if code != 1 || len(h.backend.redeemCalls) != redeemAttempts || !strings.Contains(errOut, "may or may not have been used") {
		t.Fatalf("exit %d, calls %d, err %q", code, len(h.backend.redeemCalls), errOut)
	}

	// A definite rejection isn't retried and isn't ambiguous.
	h, _ = redeemHarness(t)
	h.backend.redeemReply = []refreshReply{{400, `{"detail":"bad request"}`}}
	_, errOut, code = h.run("redeem", "work", "--yes")
	if code != 1 || len(h.backend.redeemCalls) != 1 || strings.Contains(errOut, "may or may not") {
		t.Fatalf("400: exit %d, calls %d, err %q", code, len(h.backend.redeemCalls), errOut)
	}
}

func TestRedeemOutcomes(t *testing.T) {
	tests := map[string]string{
		`{"code":"nothing_to_reset"}`: "work's usage doesn't need a reset right now",
		`{"code":"no_credit"}`:        "that reset is no longer available",
		`{"code":"surprise"}`:         `unexpected response from ChatGPT: "surprise"`,
	}
	for body, want := range tests {
		h, _ := redeemHarness(t)
		h.backend.redeemReply = []refreshReply{{200, body}}
		h.backend.onRedeem = nil
		_, errOut, code := h.run("redeem", "work", "--yes")
		if code != 1 || !strings.Contains(errOut, want) {
			t.Errorf("%s: exit %d, stderr %q", body, code, errOut)
		}
	}

	h, _ := redeemHarness(t)
	h.backend.redeemReply = []refreshReply{{200, `{"code":"already_redeemed"}`}}
	if out, _, code := h.run("redeem", "work", "--yes"); code != 0 || !strings.Contains(out, "✓ Usage limits reset for work.") {
		t.Errorf("already_redeemed: exit %d, output %q", code, out)
	}
}

func TestRedeemWithoutResets(t *testing.T) {
	h := newHarness(t)
	access := accessToken(t, "work", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", access, "rt"))
	h.backend.usage[access] = usageBody("plus", 100, 60, `, "rate_limit_reset_credits": {"available_count": 0}`)
	h.stdin = "y\n"

	_, errOut, code := h.run("redeem", "work")
	if code != 1 || !strings.Contains(errOut, "work has no usage limit resets available") || len(h.backend.redeemCalls) != 0 {
		t.Fatalf("exit %d, stderr %q, calls %v", code, errOut, h.backend.redeemCalls)
	}
	if _, errOut, code := h.run("redeem", "nobody"); code != 1 || !strings.Contains(errOut, fmt.Sprintf("no account matches %q", "nobody")) {
		t.Fatalf("unknown account: exit %d, %q", code, errOut)
	}
}
