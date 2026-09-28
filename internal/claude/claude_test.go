package claude

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

var now = time.Date(2026, 9, 28, 15, 0, 0, 0, time.UTC)

func TestParseUsage(t *testing.T) {
	// Real output from a Max account under its limits.
	u, err := ParseUsage("You are currently using your subscription to power your Claude Code usage\n\n"+
		"Current session: 4% used · resets Sep 28, 5:59pm (UTC)\n"+
		"Current week (all models): 27% used · resets Sep 29, 10pm (UTC)\n"+
		"Current week (Fable): 0% used · resets Sep 29, 10pm (UTC)\n", now)
	if err != nil {
		t.Fatal(err)
	}
	if u.Session.UsedPercent != 4 || !u.Session.ResetsAt.Equal(time.Date(2026, 9, 28, 17, 59, 0, 0, time.UTC)) {
		t.Errorf("session %+v", u.Session)
	}
	if u.Week.UsedPercent != 27 || !u.Week.ResetsAt.Equal(time.Date(2026, 9, 29, 22, 0, 0, 0, time.UTC)) {
		t.Errorf("week %+v", u.Week)
	}
	if len(u.Models) != 1 || u.Models[0].Model != "Fable" || u.Models[0].UsedPercent != 0 || len(u.Notes) != 0 {
		t.Errorf("models %+v, notes %v", u.Models, u.Notes)
	}

	// And one at its weekly limit, with no session in progress.
	u, err = ParseUsage("Current session: 0% used\nCurrent week (all models): 100% used · resets Sep 30, 8:59am (UTC)\nExtra usage: off", now)
	if err != nil {
		t.Fatal(err)
	}
	if !u.Session.ResetsAt.IsZero() || u.Week.UsedPercent != 100 || !u.Week.ResetsAt.Equal(time.Date(2026, 9, 30, 8, 59, 0, 0, time.UTC)) {
		t.Errorf("at limit: session %+v, week %+v", u.Session, u.Week)
	}
	if !slices.Equal(u.Notes, []string{"Extra usage: off"}) {
		t.Errorf("notes %v", u.Notes)
	}

	if _, err := ParseUsage("Not logged in · Please run /login", now); err == nil {
		t.Error("parsed output with no usage lines")
	}
}

func TestParseReset(t *testing.T) {
	tests := map[string]time.Time{
		"Sep 28, 6pm (UTC)":              time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC),
		"Oct 1, 9:05am (UTC)":            time.Date(2026, 10, 1, 9, 5, 0, 0, time.UTC),
		"Jan 3, 1am (UTC)":               time.Date(2027, 1, 3, 1, 0, 0, 0, time.UTC), // next year
		"Sep 28, 2pm (UTC)":              time.Date(2026, 9, 28, 14, 0, 0, 0, time.UTC),
		"11pm (UTC)":                     time.Date(2026, 9, 28, 23, 0, 0, 0, time.UTC),
		"9am (UTC)":                      time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC),
		"Sep 28, 8pm (Europe/Amsterdam)": time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC),
	}
	for in, want := range tests {
		got, err := parseReset(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseReset(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseReset("soon", now); err == nil {
		t.Error("parsed a nonsense reset time")
	}
}

func TestEnvDropsOtherCredentials(t *testing.T) {
	c := &CLI{Environ: []string{
		"PATH=/bin", "HOME=/home/me", "HTTPS_PROXY=http://proxy",
		"ANTHROPIC_API_KEY=sk", "ANTHROPIC_BASE_URL=http://gw", "CLAUDE_CODE_OAUTH_TOKEN=t",
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDECODE=1", "CLAUDE_CONFIG_DIR=/other",
	}}
	got := c.env(Profile{Label: "work", Dir: "/profiles/work"}, "TZ=UTC")
	want := []string{"PATH=/bin", "HOME=/home/me", "HTTPS_PROXY=http://proxy", "CLAUDE_CONFIG_DIR=/profiles/work", "TZ=UTC"}
	if !slices.Equal(got, want) {
		t.Errorf("env = %v", got)
	}
	if got := c.env(Profile{Label: DefaultLabel}); slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(kv, "CLAUDE_CONFIG_DIR=") }) {
		t.Errorf("default profile env sets CLAUDE_CONFIG_DIR: %v", got)
	}

	cmd, err := (&CLI{Bin: "claude", Environ: c.Environ}).Command(t.Context(), Profile{Label: "work", Dir: "/profiles/work"}, "--resume")
	if err != nil {
		t.Fatal(err)
	}
	// Starting Claude Code keeps the caller's environment, apart from the profile.
	if !slices.Contains(cmd.Env, "ANTHROPIC_API_KEY=sk") || !slices.Contains(cmd.Env, "CLAUDE_CONFIG_DIR=/profiles/work") || slices.Contains(cmd.Env, "CLAUDE_CONFIG_DIR=/other") {
		t.Errorf("run env = %v", cmd.Env)
	}
}

func TestProfilesAndLabels(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"work", "me@example.com", ".hidden", "default"} {
		os.MkdirAll(filepath.Join(root, name), 0o700)
	}
	os.WriteFile(filepath.Join(root, "current.sh"), nil, 0o600)
	got, err := Profiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Label != "me@example.com" || got[1].Label != "work" || got[1].Dir != filepath.Join(root, "work") {
		t.Fatalf("Profiles = %+v", got)
	}
	if missing, err := Profiles(filepath.Join(root, "nope")); err != nil || missing != nil {
		t.Fatalf("missing root: %v, %v", missing, err)
	}
	for _, bad := range []string{"", "../x", "a/b", ".dot", "has space", strings.Repeat("x", 65)} {
		if ValidLabel(bad) == nil {
			t.Errorf("ValidLabel(%q) accepted", bad)
		}
	}
}
