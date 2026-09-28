package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
)

// Monday 2026-09-28 12:00 UTC.
var testNow = time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

func idToken(t *testing.T, email, plan, user, workspace string) string {
	return jwt(t, map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":  plan,
			"chatgpt_user_id":    user,
			"chatgpt_account_id": workspace,
		},
	})
}

// accessToken is a JWT access token expiring at exp; name keeps tokens distinct.
func accessToken(t *testing.T, name string, exp time.Time) string {
	return jwt(t, map[string]any{"jti": name, "exp": exp.Unix()})
}

type refreshReply struct {
	status int
	body   string
}

// backend fakes auth.openai.com and the ChatGPT usage API.
type backend struct {
	t   *testing.T
	srv *httptest.Server

	mu           sync.Mutex
	loginTokens  auth.Tokens
	refreshes    map[string]refreshReply // refresh token → reply
	refreshCalls []string
	revoked      []string
	usage        map[string]string // access token → body; unknown tokens get 401
	usageCalls   []http.Header
}

func newBackend(t *testing.T) *backend {
	b := &backend{t: t, refreshes: map[string]refreshReply{}, usage: map[string]string{}}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *backend) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch r.URL.Path {
	case "/api/accounts/deviceauth/usercode":
		io.WriteString(w, `{"device_auth_id":"dev-1","user_code":"WXYZ-9876","interval":"5"}`)
	case "/api/accounts/deviceauth/token":
		io.WriteString(w, `{"authorization_code":"code","code_challenge":"ch","code_verifier":"ver"}`)
	case "/oauth/token":
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			json.NewEncoder(w).Encode(b.loginTokens)
			return
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		b.refreshCalls = append(b.refreshCalls, body["refresh_token"])
		reply, ok := b.refreshes[body["refresh_token"]]
		if !ok {
			reply = refreshReply{http.StatusBadRequest, `{"error":"refresh_token_reused"}`}
		}
		w.WriteHeader(reply.status)
		io.WriteString(w, reply.body)
	case "/oauth/revoke":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		b.revoked = append(b.revoked, body["token"])
	case "/backend-api/wham/usage":
		b.usageCalls = append(b.usageCalls, r.Header.Clone())
		body, ok := b.usage[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		io.WriteString(w, body)
	default:
		b.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func usageBody(plan string, primaryUsed, secondaryUsed float64, extra string) string {
	return fmt.Sprintf(`{
	  "plan_type": %q,
	  "rate_limit": {
	    "allowed": true, "limit_reached": false,
	    "primary_window": {"used_percent": %v, "limit_window_seconds": 18000, "reset_after_seconds": 0, "reset_at": %d},
	    "secondary_window": {"used_percent": %v, "limit_window_seconds": 604800, "reset_after_seconds": 0, "reset_at": %d}
	  }%s
	}`, plan, primaryUsed, testNow.Add(2*time.Hour+13*time.Minute).Unix(), secondaryUsed, testNow.Add(3*24*time.Hour-3*time.Hour).Unix(), extra)
}

type harness struct {
	t       *testing.T
	backend *backend
	path    string
	opened  []string
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, backend: newBackend(t), path: filepath.Join(t.TempDir(), "accounts.json")}
}

func (h *harness) run(args ...string) (stdout, stderr string, code int) {
	var out, errOut bytes.Buffer
	app := &App{
		Version: "test",
		Stdout:  &out,
		Stderr:  &errOut,
		Getenv: func(k string) string {
			switch k {
			case AuthBaseURLEnvVar:
				return h.backend.srv.URL
			case ChatGPTBaseURLEnvVar:
				return h.backend.srv.URL + "/backend-api"
			case store.HomeEnvVar:
				return filepath.Dir(h.path)
			}
			return ""
		},
		HTTPClient:  h.backend.srv.Client(),
		Now:         func() time.Time { return testNow },
		Location:    time.UTC,
		OpenBrowser: func(url string) error { h.opened = append(h.opened, url); return nil },
	}
	code = app.Run(context.Background(), args)
	return out.String(), errOut.String(), code
}

func (h *harness) seed(accounts ...store.Account) {
	h.t.Helper()
	if err := (&store.Store{Path: h.path}).Save(&store.File{Accounts: accounts}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) load() []store.Account {
	h.t.Helper()
	f, err := (&store.Store{Path: h.path}).Load()
	if err != nil {
		h.t.Fatal(err)
	}
	return f.Accounts
}

func (h *harness) account(label, email, plan, workspace, access, refresh string) store.Account {
	return store.Account{
		Label: label, Email: email, PlanType: plan, UserID: "user-" + label, AccountID: workspace,
		Tokens: auth.Tokens{
			IDToken:      idToken(h.t, email, plan, "user-"+label, workspace),
			AccessToken:  access,
			RefreshToken: refresh,
		},
		LastRefresh: testNow.Add(-time.Hour),
		AddedAt:     testNow.Add(-48 * time.Hour),
	}
}

func TestLogin(t *testing.T) {
	h := newHarness(t)
	h.backend.loginTokens = auth.Tokens{
		IDToken:      idToken(t, "me@example.com", "plus", "user-1", "ws-1"),
		AccessToken:  "access-1",
		RefreshToken: "refresh-1",
	}
	out, errOut, code := h.run("login", "--open", "--label", "personal")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{h.backend.srv.URL + "/codex/device", "WXYZ-9876", `Signed in as me@example.com (plus). Saved as personal.`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if len(h.opened) != 1 || h.opened[0] != h.backend.srv.URL+"/codex/device" {
		t.Errorf("opened %v", h.opened)
	}

	accounts := h.load()
	if len(accounts) != 1 {
		t.Fatalf("stored %d accounts", len(accounts))
	}
	got := accounts[0]
	if got.Label != "personal" || got.Email != "me@example.com" || got.PlanType != "plus" ||
		got.UserID != "user-1" || got.AccountID != "ws-1" || got.Tokens.RefreshToken != "refresh-1" ||
		!got.AddedAt.Equal(testNow) {
		t.Fatalf("stored %+v", got)
	}

	// Signing in to the same account again updates it in place.
	h.backend.loginTokens.RefreshToken = "refresh-2"
	if _, errOut, code := h.run("login"); code != 0 {
		t.Fatalf("second login exit %d: %s", code, errOut)
	}
	accounts = h.load()
	if len(accounts) != 1 || accounts[0].Label != "personal" || accounts[0].Tokens.RefreshToken != "refresh-2" {
		t.Fatalf("after re-login: %+v", accounts)
	}
}

func TestUsageTable(t *testing.T) {
	h := newHarness(t)
	workAccess := accessToken(t, "work", testNow.Add(time.Hour))
	homeAccess := accessToken(t, "home", testNow.Add(time.Hour))
	h.seed(
		h.account("work", "work@acme.com", "pro", "ws-work", workAccess, "rt-work"),
		h.account("home", "me@example.com", "plus", "ws-home", homeAccess, "rt-home"),
	)
	h.backend.usage[workAccess] = usageBody("pro", 28, 59, "")
	h.backend.usage[homeAccess] = `{
	  "plan_type": "plus",
	  "rate_limit": {"allowed": false, "limit_reached": true,
	    "primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_after_seconds": 2280, "reset_at": 0},
	    "secondary_window": {"used_percent": 88, "limit_window_seconds": 604800, "reset_at": ` + fmt.Sprint(testNow.Add(10*24*time.Hour).Unix()) + `}},
	  "credits": {"has_credits": true, "unlimited": false, "balance": "4.20"},
	  "additional_rate_limits": [{"limit_name": "Spark", "metered_feature": "spark",
	    "rate_limit": {"allowed": true, "limit_reached": false,
	      "primary_window": {"used_percent": 10, "limit_window_seconds": 3600, "reset_at": ` + fmt.Sprint(testNow.Add(30*time.Minute).Unix()) + `}}}]
	}`

	out, errOut, code := h.run("--all")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := strings.Join([]string{
		"ACCOUNT    PLAN  5H LEFT   RESETS  WEEKLY LEFT  RESETS       NOTES",
		"work       pro   72%       2h13m   41%          Thu 09:00",
		"home       plus  0%        38m     12%          Oct 8 12:00  limit reached; credits: 4.20",
		"  ↳ Spark        90% (1h)  30m     -            -",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}

	calls := h.backend.usageCalls
	if len(calls) != 2 {
		t.Fatalf("%d usage calls", len(calls))
	}
	for _, hdr := range calls {
		if hdr.Get("ChatGPT-Account-Id") == "" || hdr.Get("User-Agent") != "codex-usage/test" {
			t.Errorf("usage headers %v", hdr)
		}
	}
	if len(h.backend.refreshCalls) != 0 {
		t.Errorf("refreshed fresh tokens: %v", h.backend.refreshCalls)
	}
}

func TestUsageRefreshesExpiredTokens(t *testing.T) {
	h := newHarness(t)
	expired := accessToken(t, "old", testNow.Add(2*time.Minute)) // inside the 5 minute margin
	fresh := accessToken(t, "new", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", expired, "rt-1"))
	newID := idToken(t, "work@acme.com", "pro", "user-work", "ws-work")
	h.backend.refreshes["rt-1"] = refreshReply{200, fmt.Sprintf(`{"id_token":%q,"access_token":%q,"refresh_token":"rt-2"}`, newID, fresh)}
	h.backend.usage[fresh] = usageBody("pro", 0, 0, "")

	out, errOut, code := h.run()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, out %q", code, errOut, out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("output %q", out)
	}
	got := h.load()[0]
	if got.Tokens.RefreshToken != "rt-2" || got.Tokens.AccessToken != fresh || got.PlanType != "pro" || !got.LastRefresh.Equal(testNow) {
		t.Fatalf("stored after refresh: %+v", got)
	}

	// The rotated token is used next time; the spent one never again.
	h.run()
	if calls := h.backend.refreshCalls; len(calls) != 1 || calls[0] != "rt-1" {
		t.Fatalf("refresh calls %v", calls)
	}
}

func TestUsageMarksDeadSignIns(t *testing.T) {
	h := newHarness(t)
	fresh := accessToken(t, "ok", testNow.Add(time.Hour))
	h.seed(
		h.account("dead", "dead@example.com", "plus", "ws-dead", accessToken(t, "dead", testNow.Add(-time.Hour)), "rt-dead"),
		h.account("ok", "ok@example.com", "plus", "ws-ok", fresh, "rt-ok"),
	)
	h.backend.usage[fresh] = usageBody("plus", 50, 50, "")

	out, _, code := h.run()
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out, "sign-in was used elsewhere and is no longer valid; run `codex-usage login` to sign in again") {
		t.Errorf("output %q", out)
	}
	accounts := h.load()
	if !accounts[0].NeedsRelogin || accounts[1].NeedsRelogin {
		t.Fatalf("needs_relogin = %v, %v", accounts[0].NeedsRelogin, accounts[1].NeedsRelogin)
	}

	// Dead accounts are skipped without contacting the server again.
	h.backend.refreshCalls = nil
	out, _, _ = h.run()
	if len(h.backend.refreshCalls) != 0 || !strings.Contains(out, "signed out; run `codex-usage login`") {
		t.Fatalf("second run: refreshes %v, output %q", h.backend.refreshCalls, out)
	}
	accountsOut, _, _ := h.run("accounts")
	if !strings.Contains(accountsOut, "sign in again") {
		t.Errorf("accounts output %q", accountsOut)
	}
}

func TestUsageRetriesRejectedToken(t *testing.T) {
	h := newHarness(t)
	revoked := accessToken(t, "revoked", testNow.Add(time.Hour))
	fresh := accessToken(t, "fresh", testNow.Add(2*time.Hour))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", revoked, "rt-1"))
	h.backend.refreshes["rt-1"] = refreshReply{200, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-2"}`, fresh)}
	h.backend.usage[fresh] = usageBody("plus", 10, 20, "")

	out, errOut, code := h.run()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, out %q", code, errOut, out)
	}
	if len(h.backend.usageCalls) != 2 || len(h.backend.refreshCalls) != 1 {
		t.Fatalf("usage calls %d, refresh calls %v", len(h.backend.usageCalls), h.backend.refreshCalls)
	}
	if got := h.load()[0].Tokens.RefreshToken; got != "rt-2" {
		t.Fatalf("stored refresh token %q", got)
	}
}

func TestUsageTransientRefreshFailure(t *testing.T) {
	h := newHarness(t)
	soon := accessToken(t, "soon", testNow.Add(3*time.Minute))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", soon, "rt-1"))
	h.backend.refreshes["rt-1"] = refreshReply{503, `unavailable`}
	h.backend.usage[soon] = usageBody("plus", 10, 20, "")

	// The current token still works, so usage shows with a warning.
	out, _, code := h.run()
	if code != 0 || !strings.Contains(out, "refreshing sign-in failed: HTTP 503") {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if got := h.load()[0]; got.NeedsRelogin || got.Tokens.RefreshToken != "rt-1" {
		t.Fatalf("transient failure changed the account: %+v", got)
	}
}

func TestUsageJSON(t *testing.T) {
	h := newHarness(t)
	access := accessToken(t, "work", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "pro", "ws-work", access, "rt"))
	h.backend.usage[access] = usageBody("pro", 28, 59, `, "credits": {"has_credits": false, "unlimited": true}`)

	out, _, code := h.run("usage", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got jsonUsage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	a := got.Accounts[0]
	if a.Label != "work" || a.Plan != "pro" || a.RateLimit == nil || len(a.RateLimit.Windows) != 2 || !a.Credits.Unlimited {
		t.Fatalf("JSON account %+v", a)
	}
	w := a.RateLimit.Windows[0]
	if w.Name != "5h" || w.LeftPercent != 72 || w.ResetsAt != "2026-09-28T14:13:00Z" {
		t.Fatalf("JSON window %+v", w)
	}
	if strings.Contains(out, "rt") && strings.Contains(out, "refresh_token") {
		t.Fatal("JSON output leaks tokens")
	}
}

func TestNoAccounts(t *testing.T) {
	h := newHarness(t)
	out, errOut, code := h.run()
	if code != 0 || out != "" || !strings.Contains(errOut, "codex-usage login") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, _, _ = h.run("--json")
	if !strings.Contains(out, `"accounts": []`) {
		t.Fatalf("JSON with no accounts: %q", out)
	}
}

func TestAccountsAndLogout(t *testing.T) {
	h := newHarness(t)
	h.seed(
		h.account("work", "me@example.com", "team", "ws-team-0123456789", "a1", "rt-work"),
		h.account("home", "me@example.com", "plus", "ws-home", "a2", "rt-home"),
	)

	out, _, code := h.run("accounts")
	want := strings.Join([]string{
		"LABEL  EMAIL           PLAN  WORKSPACE      LAST REFRESH  STATUS",
		"work   me@example.com  team  ws-team-0123…  1h ago        ok",
		"home   me@example.com  plus  ws-home        1h ago        ok",
		"",
	}, "\n")
	if code != 0 || out != want {
		t.Fatalf("accounts exit %d:\n%s\nwant:\n%s", code, out, want)
	}

	_, errOut, code := h.run("logout", "me@example.com")
	if code != 1 || !strings.Contains(errOut, `matches several accounts ("work", "home")`) {
		t.Fatalf("ambiguous logout exit %d: %q", code, errOut)
	}

	out, errOut, code = h.run("logout", "work", "--config", h.path)
	if code != 0 || out != "Signed out of me@example.com (team).\n" {
		t.Fatalf("logout exit %d, out %q, err %q", code, out, errOut)
	}
	if len(h.backend.revoked) != 1 || h.backend.revoked[0] != "rt-work" {
		t.Fatalf("revoked %v", h.backend.revoked)
	}
	if accounts := h.load(); len(accounts) != 1 || accounts[0].Label != "home" {
		t.Fatalf("after logout: %+v", accounts)
	}

	if _, errOut, code := h.run("logout", "nobody"); code != 1 || !strings.Contains(errOut, `no account matches "nobody"`) {
		t.Fatalf("unknown logout exit %d: %q", code, errOut)
	}
}

func TestCommandLine(t *testing.T) {
	h := newHarness(t)
	if out, _, code := h.run("--version"); code != 0 || out != "test\n" {
		t.Fatalf("--version: %d %q", code, out)
	}
	if out, _, code := h.run("help"); code != 0 || !strings.Contains(out, "codex-usage login") {
		t.Fatalf("help: %d %q", code, out)
	}
	if _, errOut, code := h.run("frobnicate"); code != 2 || !strings.Contains(errOut, `unknown command "frobnicate"`) {
		t.Fatalf("unknown command: %d %q", code, errOut)
	}
	if _, _, code := h.run("usage", "--bogus"); code != 2 {
		t.Fatalf("bad flag exit %d", code)
	}
	if _, _, code := h.run("login", "--help"); code != 0 {
		t.Fatalf("login --help exit %d", code)
	}
	if _, _, code := h.run("logout"); code != 2 {
		t.Fatalf("logout without an account exit %d", code)
	}
	if _, err := os.Stat(h.path); !os.IsNotExist(err) {
		t.Fatalf("commands that did nothing created %s", h.path)
	}
}

func TestConcurrentRunsRefreshOnce(t *testing.T) {
	h := newHarness(t)
	expired := accessToken(t, "old", testNow.Add(-time.Minute))
	fresh := accessToken(t, "new", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "plus", "ws-work", expired, "rt-1"))
	// rt-1 works once; a second use would be refresh_token_reused.
	h.backend.refreshes["rt-1"] = refreshReply{200, fmt.Sprintf(`{"access_token":%q,"refresh_token":"rt-2"}`, fresh)}
	h.backend.usage[fresh] = usageBody("plus", 10, 20, "")

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, codes[i] = h.run()
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != 0 {
			t.Errorf("run %d exit %d", i, code)
		}
	}
	if calls := h.backend.refreshCalls; len(calls) != 1 {
		t.Fatalf("refresh calls %v, want exactly one", calls)
	}
	if got := h.load()[0]; got.NeedsRelogin || got.Tokens.RefreshToken != "rt-2" {
		t.Fatalf("stored %+v", got)
	}
}
