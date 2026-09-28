package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/codexauth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
)

func (h *harness) codex() codexauth.Home { return codexauth.Home{Dir: h.codexHome} }

// codexTokens returns the tokens in Codex's auth.json.
func (h *harness) codexTokens() auth.Tokens {
	h.t.Helper()
	a, err := h.codex().Read()
	if err != nil || a == nil || a.Tokens == nil {
		h.t.Fatalf("reading Codex auth.json: %v, %+v", err, a)
	}
	return *a.Tokens
}

func (h *harness) find(label string) store.Account {
	h.t.Helper()
	for _, a := range h.load() {
		if a.Label == label {
			return a
		}
	}
	h.t.Fatalf("no stored account %q", label)
	return store.Account{}
}

// twoAccounts seeds work (pro) and home (plus) with valid tokens and usage.
func twoAccounts(h *harness) (work, home store.Account) {
	work = h.account("work", "work@acme.com", "pro", "ws-work", accessToken(h.t, "work", testNow.Add(time.Hour)), "rt-work")
	home = h.account("home", "me@example.com", "plus", "ws-home", accessToken(h.t, "home", testNow.Add(time.Hour)), "rt-home")
	h.seed(work, home)
	h.backend.usage[work.Tokens.AccessToken] = usageBody("pro", 10, 20, "")
	h.backend.usage[home.Tokens.AccessToken] = usageBody("plus", 10, 20, "")
	return work, home
}

func TestSwitch(t *testing.T) {
	h := newHarness(t)
	work, home := twoAccounts(h)

	out, errOut, code := h.run("switch", "work")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "✓ Switched Codex to work (work@acme.com, pro).\n"+restartNote+"\n" {
		t.Fatalf("output %q", out)
	}
	if got := h.codexTokens(); got != work.Tokens {
		t.Fatalf("Codex tokens %+v", got)
	}
	a, _ := h.codex().Read()
	if a.AccountID != "ws-work" || !a.IsChatGPT() {
		t.Fatalf("auth.json %+v", a)
	}
	if !h.find("work").InCodex || h.find("home").InCodex {
		t.Fatal("in_codex not recorded")
	}
	if out, _, _ := h.run("switch"); out != "Codex is using work (work@acme.com, pro).\n" {
		t.Fatalf("switch without args: %q", out)
	}

	out, _, _ = h.run("switch", "me@example.com")
	if !strings.HasPrefix(out, "✓ Switched Codex from work to home (me@example.com, plus).\n") {
		t.Fatalf("second switch: %q", out)
	}
	if h.codexTokens() != home.Tokens || h.find("work").InCodex || !h.find("home").InCodex {
		t.Fatal("second switch didn't move Codex to home")
	}
	if out, _, _ := h.run("switch", "home"); out != "Codex is already using home (me@example.com, plus).\n" {
		t.Fatalf("repeat switch: %q", out)
	}

	usageOut, _, _ := h.run()
	if !strings.Contains(usageOut, "\nhome (codex)  ") {
		t.Fatalf("usage table doesn't mark the Codex account:\n%s", usageOut)
	}
}

func TestSwitchSavesCodexAccount(t *testing.T) {
	h := newHarness(t)
	twoAccounts(h)
	theirs := auth.Tokens{
		IDToken:      idToken(t, "solo@example.com", "plus", "user-solo", "ws-solo"),
		AccessToken:  accessToken(t, "solo", testNow.Add(time.Hour)),
		RefreshToken: "rt-solo",
	}
	if err := h.codex().SignIn(theirs, "ws-solo", testNow); err != nil {
		t.Fatal(err)
	}
	if out, _, _ := h.run("switch"); !strings.Contains(out, "isn't one of your codex-usage accounts") {
		t.Fatalf("switch without args: %q", out)
	}

	out, errOut, code := h.run("switch", "work")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.HasPrefix(out, "Saved Codex's previous account as solo@example.com (plus).\n✓ Switched Codex from solo@example.com to work") {
		t.Fatalf("output %q", out)
	}
	saved := h.find("solo@example.com")
	if saved.Tokens != theirs || saved.InCodex {
		t.Fatalf("saved %+v", saved)
	}

	// And back again.
	if _, _, code := h.run("switch", "solo@example.com"); code != 0 || h.codexTokens() != theirs {
		t.Fatalf("switching back: exit %d", code)
	}
}

func TestSwitchRefusesOtherSignIns(t *testing.T) {
	h := newHarness(t)
	twoAccounts(h)
	apiKey := `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-test"}`
	os.MkdirAll(h.codexHome, 0o700)
	os.WriteFile(h.codex().AuthPath(), []byte(apiKey), 0o600)

	_, errOut, code := h.run("switch", "work")
	if code != 1 || !strings.Contains(errOut, "Codex is signed in with an API key or another method; switching replaces it. Pass --force") {
		t.Fatalf("exit %d: %q", code, errOut)
	}
	if data, _ := os.ReadFile(h.codex().AuthPath()); string(data) != apiKey {
		t.Fatal("auth.json changed without --force")
	}

	out, _, code := h.run("switch", "work", "--force")
	backup := filepath.Join(filepath.Dir(h.path), "codex-auth.backup.json")
	if code != 0 || !strings.Contains(out, "Backed up Codex's previous sign-in to "+backup) {
		t.Fatalf("--force: exit %d, %q", code, out)
	}
	if data, _ := os.ReadFile(backup); string(data) != apiKey {
		t.Fatalf("backup = %q", data)
	}
}

func TestSwitchRefusesKeyringStorage(t *testing.T) {
	h := newHarness(t)
	twoAccounts(h)
	os.MkdirAll(h.codexHome, 0o700)
	os.WriteFile(filepath.Join(h.codexHome, "config.toml"), []byte(`cli_auth_credentials_store = "keyring"`), 0o600)
	_, errOut, code := h.run("switch", "work")
	if code != 1 || !strings.Contains(errOut, "OS keyring") {
		t.Fatalf("exit %d: %q", code, errOut)
	}
	if _, err := os.Stat(h.codex().AuthPath()); !os.IsNotExist(err) {
		t.Fatal("wrote auth.json despite keyring storage")
	}
}

func TestSwitchNeedsWorkingSignIn(t *testing.T) {
	h := newHarness(t)
	work, home := twoAccounts(h)
	work.NeedsRelogin = true
	h.seed(work, home)
	if _, errOut, code := h.run("switch", "work"); code != 1 || !strings.Contains(errOut, "work needs to sign in again first") {
		t.Fatalf("exit %d: %q", code, errOut)
	}
}

func TestCodexRefreshIsAdopted(t *testing.T) {
	h := newHarness(t)
	work, _ := twoAccounts(h)
	h.run("switch", "work")

	// Codex refreshes the shared session, rotating its tokens.
	rotated := auth.Tokens{
		IDToken:      work.Tokens.IDToken,
		AccessToken:  accessToken(t, "work-2", testNow.Add(2*time.Hour)),
		RefreshToken: "rt-work-2",
	}
	a, _ := h.codex().Read()
	h.codex().UpdateTokens(a, rotated, testNow)
	h.backend.usage[rotated.AccessToken] = usageBody("pro", 50, 50, "")

	out, _, code := h.run()
	if code != 0 || !strings.Contains(out, "50%") {
		t.Fatalf("exit %d, output:\n%s", code, out)
	}
	if got := h.find("work").Tokens; got != rotated {
		t.Fatalf("stored tokens %+v, want Codex's rotated ones", got)
	}
	if len(h.backend.refreshCalls) != 0 {
		t.Fatalf("refreshed instead of adopting Codex's tokens: %v", h.backend.refreshCalls)
	}
}

func TestOurRefreshReachesCodex(t *testing.T) {
	h := newHarness(t)
	work, home := twoAccounts(h)
	work.Tokens.AccessToken = accessToken(t, "work-old", testNow.Add(time.Minute))
	h.seed(work, home)
	h.run("switch", "work")

	fresh := accessToken(t, "work-new", testNow.Add(3*time.Hour))
	h.backend.refreshes["rt-work"] = refreshReply{200, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-work-2"}`, fresh)}
	h.backend.usage[fresh] = usageBody("pro", 0, 0, "")
	if _, errOut, code := h.run(); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if got := h.codexTokens(); got.RefreshToken != "rt-work-2" || got.AccessToken != fresh {
		t.Fatalf("Codex still has %+v; the rotated refresh token must reach it", got)
	}
}

func TestCodexSignedInElsewhere(t *testing.T) {
	h := newHarness(t)
	_, home := twoAccounts(h)
	h.run("switch", "work")
	// `codex login` with another account replaces auth.json.
	h.codex().SignIn(home.Tokens, "ws-home", testNow)

	h.run()
	if h.find("work").InCodex {
		t.Fatal("work still marked as used by Codex")
	}
	if out, _, _ := h.run(); !strings.Contains(out, "home (codex)") {
		t.Fatalf("usage doesn't mark home:\n%s", out)
	}
}

func TestLogoutLeavesCodexSessionAlone(t *testing.T) {
	h := newHarness(t)
	twoAccounts(h)
	h.run("switch", "work")
	out, _, code := h.run("logout", "work")
	if code != 0 || !strings.Contains(out, "Codex is still signed in to it") || len(h.backend.revoked) != 0 {
		t.Fatalf("exit %d, output %q, revoked %v", code, out, h.backend.revoked)
	}
	if h.codexTokens().RefreshToken != "rt-work" {
		t.Fatal("logout touched Codex's sign-in")
	}
}

func TestRotate(t *testing.T) {
	h := newHarness(t)
	work := h.account("work", "work@acme.com", "pro", "ws-work", accessToken(t, "work", testNow.Add(time.Hour)), "rt-work")
	home := h.account("home", "me@example.com", "plus", "ws-home", accessToken(t, "home", testNow.Add(time.Hour)), "rt-home")
	side := h.account("side", "side@example.com", "plus", "ws-side", accessToken(t, "side", testNow.Add(time.Hour)), "rt-side")
	h.seed(work, home, side)
	h.backend.usage[work.Tokens.AccessToken] = `{"plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true,
	  "primary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":` + fmt.Sprint(testNow.Add(48*time.Hour).Unix()) + `}}}`
	h.backend.usage[home.Tokens.AccessToken] = usageBody("plus", 10, 70, "") // tightest: weekly 30% left
	h.backend.usage[side.Tokens.AccessToken] = usageBody("plus", 40, 20, "") // tightest: 5h 60% left
	h.run("switch", "work")

	out, _, code := h.run("rotate", "--dry-run")
	if code != 0 || out != "Would switch Codex to side (side@example.com, plus): 60% left in the 5h window.\n" {
		t.Fatalf("dry run: exit %d, %q", code, out)
	}
	if h.codexTokens() != work.Tokens {
		t.Fatal("dry run switched")
	}

	out, _, code = h.run("rotate")
	if code != 0 || !strings.HasPrefix(out, "✓ Switched Codex from work to side (side@example.com, plus): 60% left in the 5h window.\n") {
		t.Fatalf("rotate: exit %d, %q", code, out)
	}
	if h.codexTokens() != side.Tokens {
		t.Fatal("rotate didn't switch to side")
	}

	out, _, _ = h.run("rotate")
	if out != "Codex is already using the account with the most usage left, side (side@example.com, plus): 60% left in the 5h window.\n" {
		t.Fatalf("second rotate: %q", out)
	}
}

func TestRotateWithNothingLeft(t *testing.T) {
	h := newHarness(t)
	work, home := twoAccounts(h)
	blocked := `{"plan_type":"plus","rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":` + fmt.Sprint(testNow.Add(time.Hour).Unix()) + `}}`
	h.backend.usage[work.Tokens.AccessToken] = blocked + `}`
	h.backend.usage[home.Tokens.AccessToken] = blocked + `, "rate_limit_reset_credits": {"available_count": 1}}`

	_, errOut, code := h.run("rotate")
	if code != 1 || !strings.Contains(errOut, "no account has usage left right now; home can use a usage limit reset: `codex-usage redeem home`") {
		t.Fatalf("exit %d: %q", code, errOut)
	}
	if _, err := os.Stat(h.codex().AuthPath()); !os.IsNotExist(err) {
		t.Fatal("rotate wrote auth.json without a candidate")
	}
}
