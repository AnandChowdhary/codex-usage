// Package cli implements the codex-usage commands.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/claude"
	"github.com/AnandChowdhary/codex-usage/internal/codexauth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

// Environment variables that point the CLI at other servers, for testing.
const (
	AuthBaseURLEnvVar    = "CODEX_USAGE_AUTH_BASE_URL"
	ChatGPTBaseURLEnvVar = "CODEX_USAGE_CHATGPT_BASE_URL"
)

const helpText = `codex-usage shows how much Codex usage is left across your ChatGPT accounts.

Usage:
  codex-usage [usage] [--json] [--all]       Show usage for every account (default)
  codex-usage login [--label NAME] [--open]  Sign in to an account with a device code
  codex-usage accounts [--json]              List signed-in accounts
  codex-usage redeem <label|email> [--yes]   Use a usage limit reset on an account
  codex-usage switch [<label|email>]         Make Codex use an account (or show which it uses)
  codex-usage rotate [--dry-run]             Switch Codex to the account with the most usage left
  codex-usage claude <command>               Claude Code profiles: login, logout, switch, rotate, run
  codex-usage logout <label|email>           Sign out and forget an account
  codex-usage version                        Print the version

Every command accepts --config PATH to use another accounts file
(default: $CODEX_USAGE_HOME/accounts.json or ~/.codex-usage/accounts.json).
`

// App holds the process environment; zero fields get working defaults.
type App struct {
	Version     string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Getenv      func(string) string
	HTTPClient  *http.Client
	Now         func() time.Time
	Location    *time.Location
	OpenBrowser func(url string) error
	Color       bool
	// Environ is the environment passed on to Claude Code.
	Environ func() []string
	// StdoutTerminal is set when stdout is a terminal rather than a pipe.
	StdoutTerminal bool
}

// ColorEnabled reports whether ANSI colors should be written to f.
func ColorEnabled(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return IsTerminal(f)
}

// IsTerminal reports whether f is a terminal.
func IsTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func (a *App) setDefaults() {
	if a.Version == "" {
		a.Version = "dev"
	}
	if a.Stdin == nil {
		a.Stdin = strings.NewReader("")
	}
	if a.Stdout == nil {
		a.Stdout = io.Discard
	}
	if a.Stderr == nil {
		a.Stderr = io.Discard
	}
	if a.Getenv == nil {
		a.Getenv = os.Getenv
	}
	if a.HTTPClient == nil {
		// chatgpt.com sets Cloudflare cookies that later requests should send back.
		jar, _ := cookiejar.New(nil)
		a.HTTPClient = &http.Client{Timeout: 30 * time.Second, Jar: jar}
	}
	if a.Now == nil {
		a.Now = time.Now
	}
	if a.Location == nil {
		a.Location = time.Local
	}
	if a.OpenBrowser == nil {
		a.OpenBrowser = openBrowser
	}
	if a.Environ == nil {
		a.Environ = os.Environ
	}
}

// Run executes the command line and returns the process exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	a.setDefaults()

	cmd := "usage"
	if len(args) > 0 {
		switch args[0] {
		case "-h", "-help", "--help", "help":
			fmt.Fprint(a.Stdout, helpText)
			return 0
		case "-version", "--version", "version":
			fmt.Fprintln(a.Stdout, a.Version)
			return 0
		}
		if !strings.HasPrefix(args[0], "-") {
			cmd, args = args[0], args[1:]
		}
	}

	var err error
	switch cmd {
	case "usage":
		err = a.runUsage(ctx, args)
	case "login":
		err = a.runLogin(ctx, args)
	case "accounts":
		err = a.runAccounts(ctx, args)
	case "logout":
		err = a.runLogout(ctx, args)
	case "redeem":
		err = a.runRedeem(ctx, args)
	case "switch":
		err = a.runSwitch(ctx, args)
	case "rotate":
		err = a.runRotate(ctx, args)
	case "claude":
		err = a.runClaude(ctx, args)
	default:
		fmt.Fprintf(a.Stderr, "codex-usage: unknown command %q\n\n%s", cmd, helpText)
		return 2
	}

	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, &exit):
		return int(exit)
	case errors.Is(err, context.Canceled):
		fmt.Fprintln(a.Stderr, "codex-usage: interrupted")
		return 130
	default:
		fmt.Fprintf(a.Stderr, "codex-usage: %v\n", err)
		return 1
	}
}

// exitError ends a command with a status code after it has reported its own
// errors.
type exitError int

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// env is what a command needs once its flags are parsed.
type env struct {
	*App
	store *store.Store
	auth  *auth.Client
	usage *usage.Client
	codex codexauth.Home

	claude     *claude.CLI
	claudeRoot string // where Claude Code profiles live
}

func (a *App) newEnv(configPath string) (*env, error) {
	if configPath == "" {
		var err error
		if configPath, err = store.DefaultPath(a.Getenv); err != nil {
			return nil, err
		}
	}
	if abs, err := filepath.Abs(configPath); err == nil {
		// Claude Code keys its credentials on the profile path, so keep it stable.
		configPath = abs
	}
	codex, err := codexauth.DefaultHome(a.Getenv)
	if err != nil {
		return nil, err
	}
	claudeBin := a.Getenv(ClaudeBinEnvVar)
	switch claudeBin {
	case "":
		claudeBin, _ = exec.LookPath("claude")
	case "off":
		claudeBin = ""
	}
	userAgent := "codex-usage/" + a.Version
	return &env{
		codex:      codex,
		claude:     &claude.CLI{Bin: claudeBin, Environ: a.Environ()},
		claudeRoot: filepath.Join(filepath.Dir(configPath), "claude"),
		App:        a,
		store:      &store.Store{Path: configPath},
		auth:       auth.NewClient(a.Getenv(AuthBaseURLEnvVar), a.HTTPClient, userAgent),
		usage:      usage.NewClient(a.Getenv(ChatGPTBaseURLEnvVar), a.HTTPClient, userAgent),
	}, nil
}

// newFlags returns a flag set with the flags every command shares.
func (a *App) newFlags(name, synopsis string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	config := fs.String("config", "", "path to the accounts file")
	fs.Usage = func() {
		fmt.Fprintf(a.Stderr, "Usage: codex-usage %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
	return fs, config
}

// parseFlags parses flags that may appear before or after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, err
			}
			return nil, exitError(2)
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait()
	return nil
}
