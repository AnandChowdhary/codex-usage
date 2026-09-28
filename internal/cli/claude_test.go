package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("CODEX_USAGE_FAKE_CLAUDE") == "1" {
		os.Exit(fakeClaude(os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeClaude stands in for the claude binary. A profile directory holds
// status.json (what `claude auth status` prints) and usage.txt (the text of
// /usage); every call is logged to $FAKE_CLAUDE_LOG.
func fakeClaude(args []string) int {
	dir := os.Getenv("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = os.Getenv("FAKE_CLAUDE_DEFAULT")
	}
	wd, _ := os.Getwd()
	call, _ := json.Marshal(map[string]any{
		"args":        args,
		"config_dir":  os.Getenv("CLAUDE_CONFIG_DIR"),
		"tz":          os.Getenv("TZ"),
		"api_key":     os.Getenv("ANTHROPIC_API_KEY") != "" || os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "",
		"working_dir": wd,
	})
	if f, err := os.OpenFile(os.Getenv("FAKE_CLAUDE_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Write(append(call, '\n'))
		f.Close()
	}

	switch {
	case len(args) >= 2 && args[0] == "auth" && args[1] == "status":
		data, err := os.ReadFile(filepath.Join(dir, "status.json"))
		if err != nil {
			fmt.Println(`{"loggedIn": false, "authMethod": "none"}`)
			return 1
		}
		os.Stdout.Write(data)
		return 0
	case len(args) >= 2 && args[0] == "auth" && args[1] == "login":
		fmt.Print("Paste code here if prompted > ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != "good-code" {
			fmt.Println("Login failed")
			return 1
		}
		os.WriteFile(filepath.Join(dir, "status.json"), []byte(os.Getenv("FAKE_CLAUDE_LOGIN_STATUS")), 0o600)
		fmt.Println("Login successful.")
		return 0
	case len(args) >= 2 && args[0] == "auth" && args[1] == "logout":
		os.Remove(filepath.Join(dir, "status.json"))
		fmt.Println("Successfully logged out from your Anthropic account.")
		return 0
	case len(args) >= 2 && args[0] == "-p" && args[1] == "/usage":
		text, err := os.ReadFile(filepath.Join(dir, "usage.txt"))
		if err != nil {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "is_error": true, "result": "Not logged in · Please run /login"})
			return 1
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "result", "is_error": false, "local_command": "usage", "num_turns": 0, "total_cost_usd": 0, "result": string(text)})
		return 0
	default:
		fmt.Printf("claude %s (profile %s)\n", strings.Join(args, " "), os.Getenv("CLAUDE_CONFIG_DIR"))
		return 7
	}
}

func claudeStatus(email, plan string) string {
	return fmt.Sprintf(`{"loggedIn": true, "authMethod": "claude.ai", "apiProvider": "firstParty", "email": %q, "orgName": "Org", "subscriptionType": %q}`, email, plan)
}

// Real /usage output from an account under its limits and one at its weekly
// limit (see TestParseUsage in internal/claude).
const (
	claudeUsageWork = "You are currently using your subscription to power your Claude Code usage\n\n" +
		"Current session: 4% used · resets Sep 28, 5:59pm (UTC)\n" +
		"Current week (all models): 27% used · resets Sep 29, 9:59pm (UTC)\n" +
		"Current week (Fable): 0% used · resets Sep 29, 10pm (UTC)"
	claudeUsageHome = "You are currently using your subscription to power your Claude Code usage\n\n" +
		"Current session: 0% used\n" +
		"Current week (all models): 100% used · resets Sep 30, 8:59am (UTC)\n" +
		"Current week (Fable): 0% used · resets Sep 30, 9am (UTC)"
)

// claudeProfile creates a profile directory; a zero status means signed out.
func (h *harness) claudeProfile(label, status, usageText string) string {
	h.t.Helper()
	h.claudeOn = true
	dir := filepath.Join(filepath.Dir(h.path), "claude", label)
	if label == "default" {
		dir = h.claudeDefault
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		h.t.Fatal(err)
	}
	if status != "" {
		os.WriteFile(filepath.Join(dir, "status.json"), []byte(status), 0o600)
	}
	if usageText != "" {
		os.WriteFile(filepath.Join(dir, "usage.txt"), []byte(usageText), 0o600)
	}
	return dir
}

type claudeCall struct {
	Args       []string `json:"args"`
	ConfigDir  string   `json:"config_dir"`
	TZ         string   `json:"tz"`
	APIKey     bool     `json:"api_key"`
	WorkingDir string   `json:"working_dir"`
}

func (h *harness) claudeCalls() []claudeCall {
	data, _ := os.ReadFile(h.claudeLog)
	var calls []claudeCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var c claudeCall
		if json.Unmarshal([]byte(line), &c) == nil {
			calls = append(calls, c)
		}
	}
	return calls
}

func TestClaudeUsage(t *testing.T) {
	h := newHarness(t)
	workDir := h.claudeProfile("work", claudeStatus("w@acme.com", "max"), claudeUsageWork)
	h.claudeProfile("home", claudeStatus("me@example.com", "max"), claudeUsageHome)
	h.claudeProfile("default", claudeStatus("main@example.com", "pro"), "Current session: 50% used · resets Sep 28, 3pm (UTC)\nCurrent week (all models): 10% used · resets Oct 1, 9am (UTC)")

	out, errOut, code := h.run()
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	want := strings.Join([]string{
		"Claude Code",
		"ACCOUNT            PLAN  5H LEFT  RESETS  WEEKLY LEFT  RESETS     NOTES",
		"default (current)  pro   50%      3h00m   90%          Thu 09:00",
		"home               max   100%     -       0%           Wed 08:59  limit reached",
		"work               max   96%      5h59m   73%          Tue 21:59",
		"",
	}, "\n")
	if out != want {
		t.Fatalf("output:\n%s\nwant:\n%s", out, want)
	}

	out, _, _ = h.run("--all")
	if !strings.Contains(out, "  ↳ Fable                -        -       100%         Tue 22:00\n") {
		t.Fatalf("--all output lacks the per-model window:\n%s", out)
	}

	for _, c := range h.claudeCalls() {
		if c.APIKey {
			t.Errorf("API key credentials reached claude: %+v", c)
		}
		if c.ConfigDir == "/wrong/profile" {
			t.Errorf("the caller's CLAUDE_CONFIG_DIR reached claude: %+v", c)
		}
		if len(c.Args) > 1 && c.Args[1] == "/usage" {
			if c.TZ != "UTC" || c.WorkingDir == "" || strings.HasPrefix(c.WorkingDir, filepath.Dir(h.path)) {
				t.Errorf("/usage call %+v", c)
			}
			if !strings.Contains(strings.Join(c.Args, " "), "--no-session-persistence --output-format json") {
				t.Errorf("/usage args %v", c.Args)
			}
		}
	}
	if !strings.Contains(out, "home               max   100%     -       0%           Wed 08:59  limit reached") {
		t.Fatalf("--all output:\n%s", out)
	}
	_ = workDir
}

func TestClaudeUsageNextToCodex(t *testing.T) {
	h := newHarness(t)
	access := accessToken(t, "work", testNow.Add(time.Hour))
	h.seed(h.account("work", "work@acme.com", "pro", "ws-work", access, "rt"))
	h.backend.usage[access] = usageBody("pro", 28, 59, "")
	h.claudeProfile("cc", claudeStatus("w@acme.com", "max"), claudeUsageWork)
	h.claudeProfile("gone", "", "")

	out, _, code := h.run()
	if code != 1 {
		t.Fatalf("a signed-out profile should fail the run; exit %d", code)
	}
	if !strings.HasPrefix(out, "Codex\nACCOUNT") || !strings.Contains(out, "\n\nClaude Code\nACCOUNT") {
		t.Fatalf("output:\n%s", out)
	}
	if !strings.Contains(out, "not signed in to a Claude plan; run `codex-usage claude login gone`") {
		t.Fatalf("signed-out profile not reported:\n%s", out)
	}

	out, _, _ = h.run("--json")
	var got jsonUsage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || len(got.Claude) != 2 {
		t.Fatalf("JSON has %d Codex accounts and %d Claude profiles", len(got.Accounts), len(got.Claude))
	}
	cc := got.Claude[0]
	if cc.Label != "cc" || cc.Plan != "max" || cc.RateLimit == nil || len(cc.RateLimit.Windows) != 2 || cc.RateLimit.Windows[1].LeftPercent != 73 || !strings.HasSuffix(cc.ConfigDir, filepath.Join("claude", "cc")) {
		t.Fatalf("Claude JSON %+v", cc)
	}

	out, _, _ = h.run("accounts")
	if !strings.Contains(out, "\n\nClaude Code\nPROFILE  EMAIL       PLAN  STATUS\ncc       w@acme.com  max   ok\n") {
		t.Fatalf("accounts output:\n%s", out)
	}
}

func TestClaudeLogin(t *testing.T) {
	h := newHarness(t)
	h.claudeOn = true
	h.environ = []string{"FAKE_CLAUDE_LOGIN_STATUS=" + claudeStatus("new@example.com", "max")}
	h.stdin = "good-code\n"
	out, errOut, code := h.run("claude", "login", "fresh")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, out %q", code, errOut, out)
	}
	if !strings.Contains(out, "Paste code here if prompted > Login successful.\n✓ Signed in Claude Code profile fresh (new@example.com, max).") {
		t.Fatalf("output %q", out)
	}
	calls := h.claudeCalls()
	if calls[0].Args[2] != "--claudeai" || !strings.HasSuffix(calls[0].ConfigDir, filepath.Join("claude", "fresh")) {
		t.Fatalf("login call %+v", calls[0])
	}

	// A failed sign-in leaves no profile behind.
	h.stdin = "wrong\n"
	if _, _, code := h.run("claude", "login", "broken"); code != 1 {
		t.Fatalf("failed login exit %d", code)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(h.path), "claude", "broken")); !os.IsNotExist(err) {
		t.Fatal("failed login left its profile directory")
	}
	if _, errOut, code := h.run("claude", "login", "../escape"); code != 1 || !strings.Contains(errOut, "invalid profile name") {
		t.Fatalf("bad name: exit %d, %q", code, errOut)
	}
}

func TestClaudeSwitch(t *testing.T) {
	h := newHarness(t)
	workDir := h.claudeProfile("work", claudeStatus("w@acme.com", "max"), claudeUsageWork)
	h.claudeProfile("gone", "", "")
	root := filepath.Dir(workDir)

	out, errOut, code := h.run("claude", "switch", "work")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if out != "export CLAUDE_CONFIG_DIR='"+workDir+"'\n" {
		t.Fatalf("stdout %q", out)
	}
	if !strings.Contains(errOut, "✓ Switched Claude Code to work (w@acme.com, max) for new shells.") ||
		!strings.Contains(errOut, ". '"+filepath.Join(root, "current.sh")+"'") {
		t.Fatalf("stderr %q", errOut)
	}
	sh, _ := os.ReadFile(filepath.Join(root, "current.sh"))
	if !strings.Contains(string(sh), "export CLAUDE_CONFIG_DIR='"+workDir+"'\nexport CODEX_USAGE_CLAUDE_PROFILE='work'\n") {
		t.Fatalf("current.sh = %q", sh)
	}
	if out, _, _ := h.run("claude", "switch"); out != "New shells use Claude Code profile work.\n" {
		t.Fatalf("switch without a profile: %q", out)
	}
	if out, _, _ := h.run(); !strings.Contains(out, "work (current)") {
		t.Fatalf("usage doesn't mark the current profile:\n%s", out)
	}

	// Once shells source current.sh, the setup hint goes away; fish gets fish syntax.
	h.getenv["CODEX_USAGE_CLAUDE_PROFILE"] = "work"
	h.getenv["SHELL"] = "/usr/local/bin/fish"
	out, errOut, _ = h.run("claude", "switch", "work")
	if out != "set -gx CLAUDE_CONFIG_DIR '"+workDir+"'\n" || strings.Contains(errOut, "startup file") {
		t.Fatalf("fish: stdout %q, stderr %q", out, errOut)
	}
	h.getenv["SHELL"] = "/bin/zsh"
	h.claudeProfile("default", claudeStatus("main@example.com", "pro"), "")
	if out, _, _ := h.run("claude", "switch", "default"); out != "unset CLAUDE_CONFIG_DIR\n" {
		t.Fatalf("switch default: %q", out)
	}

	if _, errOut, code := h.run("claude", "switch", "gone"); code != 1 || !strings.Contains(errOut, "gone isn't signed in to a Claude plan") {
		t.Fatalf("signed-out profile: exit %d, %q", code, errOut)
	}
	if _, errOut, code := h.run("claude", "switch", "nope"); code != 1 || !strings.Contains(errOut, `no Claude Code profile "nope"`) {
		t.Fatalf("missing profile: exit %d, %q", code, errOut)
	}
}

func TestClaudeRotate(t *testing.T) {
	h := newHarness(t)
	workDir := h.claudeProfile("work", claudeStatus("w@acme.com", "max"), claudeUsageWork) // tightest: 73% weekly
	h.claudeProfile("home", claudeStatus("me@example.com", "max"), claudeUsageHome)        // at its limit
	h.claudeProfile("default", claudeStatus("main@example.com", "pro"), "Current session: 60% used · resets Sep 28, 3pm (UTC)\nCurrent week (all models): 10% used · resets Oct 1, 9am (UTC)")

	out, errOut, code := h.run("claude", "rotate", "--dry-run")
	if code != 0 || out != "" || !strings.Contains(errOut, "Would switch Claude Code to work (w@acme.com, max): 73% left in the weekly window.") {
		t.Fatalf("dry run: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, code = h.run("claude", "rotate")
	if code != 0 || out != "export CLAUDE_CONFIG_DIR='"+workDir+"'\n" || !strings.Contains(errOut, "Switched Claude Code from default to work") {
		t.Fatalf("rotate: exit %d, stdout %q, stderr %q", code, out, errOut)
	}
	out, errOut, _ = h.run("claude", "rotate")
	if out != "export CLAUDE_CONFIG_DIR='"+workDir+"'\n" || !strings.Contains(errOut, "already uses the profile with the most usage left, work") {
		t.Fatalf("second rotate: %q, %q", out, errOut)
	}
	if out, _, _ := h.run("claude", "rotate", "--dry-run"); out != "" {
		t.Fatalf("dry run printed a command: %q", out)
	}

	h2 := newHarness(t)
	h2.claudeProfile("home", claudeStatus("me@example.com", "max"), claudeUsageHome)
	if _, errOut, code := h2.run("claude", "rotate"); code != 1 || !strings.Contains(errOut, "no Claude Code profile has usage left") {
		t.Fatalf("nothing left: exit %d, %q", code, errOut)
	}
}

func TestClaudeRunAndLogout(t *testing.T) {
	h := newHarness(t)
	workDir := h.claudeProfile("work", claudeStatus("w@acme.com", "max"), claudeUsageWork)

	out, _, code := h.run("claude", "run", "work", "--resume", "abc")
	if code != 7 || out != "claude --resume abc (profile "+workDir+")\n" {
		t.Fatalf("run: exit %d, %q", code, out)
	}

	h.run("claude", "switch", "work")
	h.stdin = "n\n"
	if out, _, _ := h.run("claude", "logout", "work"); !strings.HasSuffix(out, "Cancelled.\n") {
		t.Fatalf("cancelled logout: %q", out)
	}
	h.stdin = "y\n"
	out, errOut, code := h.run("claude", "logout", "work")
	if code != 0 || !strings.Contains(out, "Signed out of Claude Code profile work.\nNew shells now use Claude Code's default sign-in.") {
		t.Fatalf("logout: exit %d, %q, %q", code, out, errOut)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Fatal("logout left the profile directory")
	}
	var loggedOut bool
	for _, c := range h.claudeCalls() {
		loggedOut = loggedOut || (len(c.Args) == 2 && c.Args[1] == "logout" && c.ConfigDir == workDir)
	}
	if !loggedOut {
		t.Fatal("logout didn't sign out with Claude Code first")
	}
}
