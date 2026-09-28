package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/AnandChowdhary/codex-usage/internal/claude"
	"github.com/AnandChowdhary/codex-usage/internal/store"
	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

// ClaudeBinEnvVar overrides the Claude Code binary; "off" turns Claude Code
// support off.
const ClaudeBinEnvVar = "CODEX_USAGE_CLAUDE_BIN"

const claudeHelpText = `Manage Claude Code profiles: separate sign-ins, each in its own
CLAUDE_CONFIG_DIR. Everything runs the unmodified claude binary; codex-usage
never sees Claude credentials.

Usage:
  codex-usage claude login <profile> [--email E]   Sign a profile in with Claude Code
  codex-usage claude logout <profile> [--yes]      Sign a profile out and delete it
  codex-usage claude switch [<profile>]            Use a profile in new shells
  codex-usage claude rotate [--dry-run]            Switch to the profile with the most usage left
  codex-usage claude run <profile> [args...]       Start Claude Code in a profile

"default" is Claude Code's own sign-in (~/.claude). Usage for every profile
appears in ` + "`codex-usage`" + ` and ` + "`codex-usage accounts`" + `.

switch and rotate print a shell command; run them with eval to switch the
current shell too:  eval "$(codex-usage claude switch work)"
`

// claudeResult is a usage result for a Claude Code profile.
type claudeResult struct {
	result
	profile claude.Profile
	status  *claude.Status
}

func (a *App) runClaude(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprint(a.Stdout, claudeHelpText)
		return nil
	}
	sub, args := args[0], args[1:]
	switch sub {
	case "login":
		return a.runClaudeLogin(ctx, args)
	case "logout":
		return a.runClaudeLogout(ctx, args)
	case "switch", "use":
		return a.runClaudeSwitch(ctx, args)
	case "rotate":
		return a.runClaudeRotate(ctx, args)
	case "run":
		return a.runClaudeRun(ctx, args)
	default:
		fmt.Fprintf(a.Stderr, "codex-usage: unknown claude command %q\n\n%s", sub, claudeHelpText)
		return exitError(2)
	}
}

// claudeProfile resolves a profile name. Profiles other than "default" must
// exist unless create is set.
func (e *env) claudeProfile(label string, create bool) (claude.Profile, error) {
	if label == claude.DefaultLabel {
		return claude.Profile{Label: label}, nil
	}
	if err := claude.ValidLabel(label); err != nil {
		return claude.Profile{}, err
	}
	p := claude.Profile{Label: label, Dir: filepath.Join(e.claudeRoot, label)}
	if !create {
		if info, err := os.Stat(p.Dir); err != nil || !info.IsDir() {
			return claude.Profile{}, fmt.Errorf("no Claude Code profile %q; add it with `codex-usage claude login %s`", label, label)
		}
	}
	return p, nil
}

// collectClaude reads usage for every profile, plus Claude Code's default
// sign-in when it's signed in to a Claude plan.
func (e *env) collectClaude(ctx context.Context) ([]claudeResult, error) {
	return e.claudeProfiles(ctx, true)
}

// claudeProfiles checks every profile's sign-in, and its usage if withUsage.
func (e *env) claudeProfiles(ctx context.Context, withUsage bool) ([]claudeResult, error) {
	profiles, err := claude.Profiles(e.claudeRoot)
	if err != nil {
		return nil, err
	}
	if e.claude.Bin != "" {
		profiles = append([]claude.Profile{{Label: claude.DefaultLabel}}, profiles...)
	}
	results := make([]claudeResult, len(profiles))
	var wg sync.WaitGroup
	for i, p := range profiles {
		wg.Add(1)
		go func(r *claudeResult) {
			defer wg.Done()
			e.checkClaude(ctx, r, p, withUsage)
		}(&results[i])
	}
	wg.Wait()

	// Only show the default sign-in if it's a Claude plan.
	return slices.DeleteFunc(results, func(r claudeResult) bool {
		return r.profile.IsDefault() && (r.status == nil || !r.status.Subscription())
	}), nil
}

func (e *env) checkClaude(ctx context.Context, r *claudeResult, p claude.Profile, withUsage bool) {
	r.profile = p
	r.account = store.Account{Label: p.Label}
	r.status, r.err = e.claude.Status(ctx, p)
	if r.err != nil {
		return
	}
	r.account.Email, r.account.PlanType = r.status.Email, r.status.SubscriptionType
	if !r.status.Subscription() {
		r.err = fmt.Errorf("not signed in to a Claude plan; run `codex-usage claude login %s`", p.Label)
		return
	}
	if !withUsage {
		return
	}
	u, err := e.claude.Usage(ctx, p, e.Now())
	if err != nil {
		r.err = err
		return
	}
	r.usage = claudeResponse(u, r.status.SubscriptionType)
	r.notes = u.Notes
}

// claudeResponse maps Claude Code's /usage report onto the usage types the
// table, JSON and rotate already understand.
func claudeResponse(u *claude.Usage, plan string) *usage.Response {
	window := func(w *claude.Window, seconds int64) *usage.Window {
		if w == nil {
			return nil
		}
		out := &usage.Window{UsedPercent: w.UsedPercent, LimitWindowSeconds: seconds}
		if !w.ResetsAt.IsZero() {
			out.ResetAt = w.ResetsAt.Unix()
		}
		return out
	}
	blocked := (u.Session != nil && u.Session.UsedPercent >= 100) || (u.Week != nil && u.Week.UsedPercent >= 100)
	resp := &usage.Response{
		PlanType: plan,
		RateLimit: &usage.RateLimit{
			Allowed:         !blocked,
			LimitReached:    blocked,
			PrimaryWindow:   window(u.Session, 5*3600),
			SecondaryWindow: window(u.Week, 7*24*3600),
		},
	}
	for _, m := range u.Models {
		m := m
		resp.AdditionalRateLimits = append(resp.AdditionalRateLimits, usage.AdditionalRateLimit{
			LimitName: m.Model,
			RateLimit: &usage.RateLimit{
				Allowed:       m.UsedPercent < 100,
				LimitReached:  m.UsedPercent >= 100,
				PrimaryWindow: window(&m, 7*24*3600),
			},
		})
	}
	return resp
}

// currentClaude is the profile new shells use, as set by `claude switch`.
func (e *env) currentClaude() string {
	data, err := os.ReadFile(filepath.Join(e.claudeRoot, "current"))
	if err != nil {
		return claude.DefaultLabel
	}
	if label := strings.TrimSpace(string(data)); label != "" {
		return label
	}
	return claude.DefaultLabel
}

// setCurrentClaude records p as the profile for new shells, writing the
// snippets shells source at startup.
func (e *env) setCurrentClaude(p claude.Profile) error {
	if err := os.MkdirAll(e.claudeRoot, 0o700); err != nil {
		return err
	}
	header := "Written by `codex-usage claude switch`; source it from your shell's startup file."
	files := map[string]string{
		"current":      p.Label + "\n",
		"current.sh":   "# " + header + "\n" + shellExport(p, "sh") + "\nexport CODEX_USAGE_CLAUDE_PROFILE=" + shellQuote(p.Label) + "\n",
		"current.fish": "# " + header + "\n" + shellExport(p, "fish") + "\nset -gx CODEX_USAGE_CLAUDE_PROFILE " + shellQuote(p.Label) + "\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(e.claudeRoot, name), []byte(content), 0o600); err != nil {
			return fmt.Errorf("saving the current Claude profile: %w", err)
		}
	}
	return nil
}

func (e *env) shell() string {
	if filepath.Base(e.Getenv("SHELL")) == "fish" {
		return "fish"
	}
	return "sh"
}

// shellExport is the command that points a shell at p.
func shellExport(p claude.Profile, shell string) string {
	switch {
	case shell == "fish" && p.IsDefault():
		return "set -e " + claude.ConfigDirEnvVar
	case shell == "fish":
		return "set -gx " + claude.ConfigDirEnvVar + " " + shellQuote(p.Dir)
	case p.IsDefault():
		return "unset " + claude.ConfigDirEnvVar
	default:
		return "export " + claude.ConfigDirEnvVar + "=" + shellQuote(p.Dir)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// switchClaude makes p the profile for new shells and prints the command
// that switches the current one. Messages go to stderr so stdout can be
// passed to eval.
func (e *env) switchClaude(p claude.Profile, message string) error {
	if err := e.setCurrentClaude(p); err != nil {
		return err
	}
	fmt.Fprintln(e.Stdout, shellExport(p, e.shell()))
	fmt.Fprintln(e.Stderr, message)
	st := e.styles()
	if e.Getenv("CODEX_USAGE_CLAUDE_PROFILE") == "" {
		src := ". " + shellQuote(filepath.Join(e.claudeRoot, "current.sh"))
		if e.shell() == "fish" {
			src = "source " + shellQuote(filepath.Join(e.claudeRoot, "current.fish"))
		}
		fmt.Fprintf(e.Stderr, "%s\n  %s\n", st.dim("To have new shells follow `claude switch`, add this line to your shell's startup file (e.g. ~/.zshrc):"), src)
	}
	if e.StdoutTerminal {
		fmt.Fprintln(e.Stderr, st.dim(fmt.Sprintf("To switch this shell too, run: eval \"$(codex-usage claude switch %s)\"", p.Label)))
	}
	return nil
}

func claudeWho(r claudeResult) string {
	a := r.account
	var details []string
	if a.Email != "" {
		details = append(details, a.Email)
	}
	if a.PlanType != "" {
		details = append(details, a.PlanType)
	}
	if len(details) == 0 {
		return a.Label
	}
	return a.Label + " (" + strings.Join(details, ", ") + ")"
}

func (a *App) runClaudeLogin(ctx context.Context, args []string) error {
	fs, config := a.newFlags("claude login", "claude login <profile> [--email E]")
	email := fs.String("email", "", "pre-fill this email on the sign-in page")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return exitError(2)
	}
	e, err := a.newEnv(*config)
	if err != nil {
		return err
	}
	p, err := e.claudeProfile(positional[0], true)
	if err != nil {
		return err
	}
	created := false
	if !p.IsDefault() {
		if _, err := os.Stat(p.Dir); errors.Is(err, os.ErrNotExist) {
			created = true
		}
		if err := os.MkdirAll(p.Dir, 0o700); err != nil {
			return err
		}
	}
	if err := e.claude.Login(ctx, p, *email, e.Stdin, e.Stdout, e.Stderr); err != nil {
		if created {
			os.RemoveAll(p.Dir)
		}
		return err
	}
	status, err := e.claude.Status(ctx, p)
	if err != nil {
		return err
	}
	r := claudeResult{profile: p, status: status, result: result{account: store.Account{Label: p.Label, Email: status.Email, PlanType: status.SubscriptionType}}}
	if !status.Subscription() {
		return fmt.Errorf("%s isn't signed in to a Claude plan (sign-in method: %s)", p.Label, status.AuthMethod)
	}
	st := e.styles()
	fmt.Fprintf(e.Stdout, "%s Signed in Claude Code profile %s.\n", st.good("✓"), claudeWho(r))
	fmt.Fprintln(e.Stdout, st.dim(fmt.Sprintf("Use it with `codex-usage claude switch %s`, or start it with `codex-usage claude run %s`.", p.Label, p.Label)))
	return nil
}

func (a *App) runClaudeLogout(ctx context.Context, args []string) error {
	fs, config := a.newFlags("claude logout", "claude logout <profile> [--yes]")
	yes := fs.Bool("yes", false, "don't ask for confirmation")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fs.Usage()
		return exitError(2)
	}
	e, err := a.newEnv(*config)
	if err != nil {
		return err
	}
	p, err := e.claudeProfile(positional[0], false)
	if err != nil {
		return err
	}
	if !*yes {
		question := fmt.Sprintf("Sign Claude Code profile %s out and delete %s, including its Claude Code settings and history? [y/N] ", p.Label, p.Dir)
		if p.IsDefault() {
			question = "Sign out Claude Code's default sign-in (~/.claude)? [y/N] "
		}
		fmt.Fprint(e.Stdout, question)
		if !confirm(e.Stdin, e.Stdout) {
			fmt.Fprintln(e.Stdout, "Cancelled.")
			return nil
		}
	}
	if err := e.claude.Logout(ctx, p); err != nil {
		if p.IsDefault() {
			return err
		}
		fmt.Fprintf(e.Stderr, "Couldn't sign out with Claude Code (%v); deleting the profile anyway.\n", err)
	}
	if !p.IsDefault() {
		if err := os.RemoveAll(p.Dir); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.Stdout, "Signed out of Claude Code profile %s.\n", p.Label)
	if !p.IsDefault() && e.currentClaude() == p.Label {
		if err := e.setCurrentClaude(claude.Profile{Label: claude.DefaultLabel}); err != nil {
			return err
		}
		fmt.Fprintln(e.Stdout, "New shells now use Claude Code's default sign-in.")
	}
	return nil
}

func (a *App) runClaudeSwitch(ctx context.Context, args []string) error {
	fs, config := a.newFlags("claude switch", "claude switch [<profile>]")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 1 {
		fs.Usage()
		return exitError(2)
	}
	e, err := a.newEnv(*config)
	if err != nil {
		return err
	}
	if len(positional) == 0 {
		fmt.Fprintf(e.Stdout, "New shells use Claude Code profile %s.\n", e.currentClaude())
		return nil
	}
	p, err := e.claudeProfile(positional[0], false)
	if err != nil {
		return err
	}
	var r claudeResult
	r.profile, r.account = p, store.Account{Label: p.Label}
	r.status, err = e.claude.Status(ctx, p)
	if err != nil {
		return err
	}
	if !r.status.Subscription() {
		return fmt.Errorf("%s isn't signed in to a Claude plan; run `codex-usage claude login %s`", p.Label, p.Label)
	}
	r.account.Email, r.account.PlanType = r.status.Email, r.status.SubscriptionType
	return e.switchClaude(p, fmt.Sprintf("%s Switched Claude Code to %s for new shells.", e.styles().good("✓"), claudeWho(r)))
}

func (a *App) runClaudeRotate(ctx context.Context, args []string) error {
	fs, config := a.newFlags("claude rotate", "claude rotate [--dry-run]")
	dryRun := fs.Bool("dry-run", false, "show which profile would be used without switching")
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		fs.Usage()
		return exitError(2)
	}
	e, err := a.newEnv(*config)
	if err != nil {
		return err
	}
	results, err := e.collectClaude(ctx)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return errors.New("no Claude Code profiles yet; add one with `codex-usage claude login <profile>`")
	}
	var best *claudeResult
	var c candidate
	for i := range results {
		if cand, ok := rotationCandidate(results[i].result); ok && (best == nil || cand.left > c.left) {
			best, c = &results[i], cand
		}
	}
	if best == nil {
		return errors.New("no Claude Code profile has usage left right now")
	}
	detail := fmt.Sprintf(": %.0f%% left in the %s window", c.left, c.window)
	if best.profile.Label == e.currentClaude() {
		if !*dryRun {
			fmt.Fprintln(e.Stdout, shellExport(best.profile, e.shell()))
		}
		fmt.Fprintf(e.Stderr, "Claude Code already uses the profile with the most usage left, %s%s.\n", claudeWho(*best), detail)
		return nil
	}
	if *dryRun {
		fmt.Fprintf(e.Stderr, "Would switch Claude Code to %s%s.\n", claudeWho(*best), detail)
		return nil
	}
	return e.switchClaude(best.profile, fmt.Sprintf("%s Switched Claude Code from %s to %s%s.", e.styles().good("✓"), e.currentClaude(), claudeWho(*best), detail))
}

func (a *App) runClaudeRun(ctx context.Context, args []string) error {
	fs, config := a.newFlags("claude run", "claude run <profile> [claude arguments...]")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return exitError(2)
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return exitError(2)
	}
	e, err := a.newEnv(*config)
	if err != nil {
		return err
	}
	p, err := e.claudeProfile(fs.Arg(0), false)
	if err != nil {
		return err
	}
	cmd, err := e.claude.Command(ctx, p, fs.Args()[1:]...)
	if err != nil {
		return err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = e.Stdin, e.Stdout, e.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError(exit.ExitCode())
		}
		return err
	}
	return nil
}

// claudeLabeler marks the profile new shells use.
func (e *env) claudeLabeler() func(result) string {
	current := e.currentClaude()
	return func(r result) string {
		if r.account.Label == current {
			return r.account.Label + " (current)"
		}
		return r.account.Label
	}
}
