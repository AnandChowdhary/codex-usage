// Package claude manages Claude Code profiles: separate CLAUDE_CONFIG_DIR
// directories, each signed in to one Claude account.
//
// Anthropic doesn't allow third-party tools to handle Claude sign-in or
// store Claude credentials, so this package never does. Signing in, reading
// usage and signing out all run the unmodified `claude` binary, which keeps
// its credentials to itself.
package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// ConfigDirEnvVar selects a Claude Code profile.
	ConfigDirEnvVar = "CLAUDE_CONFIG_DIR"
	// DefaultLabel names the sign-in Claude Code uses without CLAUDE_CONFIG_DIR.
	DefaultLabel = "default"

	commandTimeout = time.Minute
)

var labelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}$`)

// ValidLabel reports whether label can name a profile directory.
func ValidLabel(label string) error {
	if !labelPattern.MatchString(label) {
		return fmt.Errorf("invalid profile name %q: use letters, digits and . _ @ + - (up to 64 characters)", label)
	}
	return nil
}

// Profile is one Claude Code sign-in.
type Profile struct {
	Label string
	Dir   string // CLAUDE_CONFIG_DIR; empty for the default sign-in
}

// IsDefault reports whether p is Claude Code's default sign-in.
func (p Profile) IsDefault() bool { return p.Dir == "" }

// Profiles lists the profile directories under root, sorted by name.
func Profiles(root string) ([]Profile, error) {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Claude profiles: %w", err)
	}
	var out []Profile
	for _, e := range entries {
		if e.IsDir() && ValidLabel(e.Name()) == nil && e.Name() != DefaultLabel {
			out = append(out, Profile{Label: e.Name(), Dir: filepath.Join(root, e.Name())})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// Status is the output of `claude auth status`.
type Status struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
}

// Subscription reports whether the profile is signed in with a Claude plan
// (rather than an API key or not at all).
func (s *Status) Subscription() bool { return s.LoggedIn && s.AuthMethod == "claude.ai" }

// CLI runs the Claude Code binary.
type CLI struct {
	Bin     string   // path to `claude`
	Environ []string // environment to start from
}

// env is the environment for running claude in p. Variables that would
// point Claude Code at an API key, gateway, cloud provider or the session
// this tool may itself be running in are dropped, so commands always act on
// the profile's own subscription sign-in.
func (c *CLI) env(p Profile, extra ...string) []string {
	var out []string
	for _, kv := range c.Environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == ConfigDirEnvVar || k == "CLAUDECODE" || strings.HasPrefix(k, "ANTHROPIC_") || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		out = append(out, kv)
	}
	if !p.IsDefault() {
		out = append(out, ConfigDirEnvVar+"="+p.Dir)
	}
	return append(out, extra...)
}

func (c *CLI) check() error {
	if c.Bin == "" {
		return errors.New("Claude Code isn't installed (no `claude` on PATH)")
	}
	return nil
}

// run runs claude non-interactively from an empty directory, so no project
// settings apply.
func (c *CLI) run(ctx context.Context, p Profile, extraEnv []string, args ...string) ([]byte, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "codex-usage-claude-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir = dir
	cmd.Env = c.env(p, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err != nil && stdout.Len() == 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("claude %s: %s", strings.Join(args[:min(2, len(args))], " "), msg)
	}
	return stdout.Bytes(), nil
}

// Status runs `claude auth status`.
func (c *CLI) Status(ctx context.Context, p Profile) (*Status, error) {
	out, err := c.run(ctx, p, nil, "auth", "status")
	if err != nil {
		return nil, err
	}
	var s Status
	if err := json.Unmarshal(out, &s); err != nil {
		return nil, fmt.Errorf("reading `claude auth status`: %w", err)
	}
	return &s, nil
}

// Login runs `claude auth login` interactively: Claude Code shows its own
// sign-in link and asks for the code.
func (c *CLI) Login(ctx context.Context, p Profile, email string, stdin io.Reader, stdout, stderr io.Writer) error {
	if err := c.check(); err != nil {
		return err
	}
	args := []string{"auth", "login", "--claudeai"}
	if email != "" {
		args = append(args, "--email", email)
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = c.env(p)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("Claude Code sign-in didn't complete: %w", err)
	}
	return nil
}

// Logout runs `claude auth logout`.
func (c *CLI) Logout(ctx context.Context, p Profile) error {
	_, err := c.run(ctx, p, nil, "auth", "logout")
	return err
}

// Command returns a command that starts Claude Code in p, keeping the rest
// of the caller's environment.
func (c *CLI) Command(ctx context.Context, p Profile, args ...string) (*exec.Cmd, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	for _, kv := range c.Environ {
		if !strings.HasPrefix(kv, ConfigDirEnvVar+"=") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	if !p.IsDefault() {
		cmd.Env = append(cmd.Env, ConfigDirEnvVar+"="+p.Dir)
	}
	return cmd, nil
}

// Window is one usage window from `/usage`.
type Window struct {
	Model       string // set for windows that count one model only
	UsedPercent float64
	ResetsAt    time.Time // zero if not reported (e.g. no session in progress)
}

// Usage is what Claude Code's `/usage` command reports.
type Usage struct {
	Session *Window  // the 5-hour window
	Week    *Window  // the weekly window across all models
	Models  []Window // weekly windows for single models
	Notes   []string // lines this parser doesn't recognise
}

// Usage runs Claude Code's `/usage` command headlessly. It's answered
// locally from Claude Code's own usage data: no model request is made.
func (c *CLI) Usage(ctx context.Context, p Profile, now time.Time) (*Usage, error) {
	out, err := c.run(ctx, p, []string{"TZ=UTC"},
		"-p", "/usage",
		"--model", "haiku", "--tools", "", "--strict-mcp-config",
		"--no-session-persistence", "--output-format", "json")
	if err != nil {
		return nil, err
	}
	var res struct {
		Result       string  `json:"result"`
		IsError      bool    `json:"is_error"`
		LocalCommand any     `json:"local_command"` // the command's name, e.g. "usage"
		NumTurns     int     `json:"num_turns"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, fmt.Errorf("reading `claude -p /usage`: %w", err)
	}
	if res.IsError {
		return nil, fmt.Errorf("claude /usage: %s", firstLine(res.Result))
	}
	u, err := ParseUsage(res.Result, now)
	if err != nil {
		return nil, err
	}
	if local := res.LocalCommand != nil && res.LocalCommand != "" && res.LocalCommand != false; !local && (res.NumTurns > 0 || res.TotalCostUSD > 0) {
		u.Notes = append(u.Notes, "this Claude Code answered /usage with a model request")
	}
	return u, nil
}

var usageLine = regexp.MustCompile(`^Current (session|week)(?: \(([^)]*)\))?:\s*([0-9]+(?:\.[0-9]+)?)% used(?:\s*·\s*resets (.+))?$`)

// ParseUsage reads the text of Claude Code's `/usage` command, e.g.
//
//	Current session: 4% used · resets Sep 28, 6pm (UTC)
//	Current week (all models): 27% used · resets Sep 29, 10pm (UTC)
//	Current week (Fable): 0% used · resets Sep 29, 10pm (UTC)
func ParseUsage(text string, now time.Time) (*Usage, error) {
	u := &Usage{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "You are currently using") {
			continue
		}
		m := usageLine.FindStringSubmatch(line)
		if m == nil {
			u.Notes = append(u.Notes, line)
			continue
		}
		used, _ := strconv.ParseFloat(m[3], 64)
		w := Window{UsedPercent: used}
		if m[4] != "" {
			w.ResetsAt, _ = parseReset(m[4], now)
		}
		switch {
		case m[1] == "session":
			u.Session = &w
		case m[2] == "" || m[2] == "all models":
			u.Week = &w
		default:
			w.Model = m[2]
			u.Models = append(u.Models, w)
		}
	}
	if u.Session == nil && u.Week == nil && len(u.Models) == 0 {
		return nil, fmt.Errorf("couldn't read Claude Code's /usage output: %q", firstLine(text))
	}
	return u, nil
}

var resetZone = regexp.MustCompile(`\s*\(([^)]*)\)\s*$`)

// parseReset reads times like "Sep 29, 10pm (UTC)" or "Sep 30, 8:59am (UTC)".
// They have no year, so the next matching date is used.
func parseReset(s string, now time.Time) (time.Time, error) {
	loc := time.UTC
	if m := resetZone.FindStringSubmatch(s); m != nil {
		if m[1] != "" && m[1] != "UTC" {
			if l, err := time.LoadLocation(m[1]); err == nil {
				loc = l
			}
		}
		s = s[:len(s)-len(m[0])]
	}
	s = strings.TrimSpace(s)
	for _, layout := range []string{"Jan 2, 3:04pm", "Jan 2, 3pm", "Jan 2 3:04pm", "Jan 2 3pm"} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		t = time.Date(now.In(loc).Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if t.Before(now.Add(-24 * time.Hour)) {
			t = t.AddDate(1, 0, 0)
		}
		return t, nil
	}
	for _, layout := range []string{"3:04pm", "3pm"} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		n := now.In(loc)
		t = time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), 0, 0, loc)
		if t.Before(now) {
			t = t.AddDate(0, 0, 1)
		}
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognised reset time %q", s)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
