package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/codexauth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
)

const restartNote = "Running Codex sessions (CLI, IDE extension, app) keep using the previous account until you restart them."

// codexAccount turns Codex's sign-in into an account record.
func codexAccount(a *codexauth.Auth) (store.Account, error) {
	id, err := auth.ParseIdentity(a.Tokens.IDToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("reading Codex's ID token: %w", err)
	}
	acct := store.Account{Tokens: *a.Tokens, LastRefresh: a.LastRefresh}
	acct.ApplyIdentity(id)
	if acct.AccountID == "" {
		acct.AccountID = a.AccountID
	}
	return acct, nil
}

// codexKey is the key of the ChatGPT account Codex is signed in to, or "".
func (e *env) codexKey() string {
	cur, err := e.codex.Read()
	if err != nil || cur == nil || !cur.IsChatGPT() {
		return ""
	}
	acct, err := codexAccount(cur)
	if err != nil {
		return ""
	}
	return acct.Key()
}

// newer reports whether tokens a were issued after tokens b, comparing access
// token expiry and falling back to the last refresh time.
func newer(a auth.Tokens, aRefreshed time.Time, b auth.Tokens, bRefreshed time.Time) bool {
	ea, okA := auth.TokenExpiry(a.AccessToken)
	eb, okB := auth.TokenExpiry(b.AccessToken)
	if okA && okB && !ea.Equal(eb) {
		return ea.After(eb)
	}
	return aRefreshed.After(bRefreshed)
}

// reconcileCodex keeps the account handed to Codex in step with auth.json.
// Either side may have refreshed (and so rotated) the shared session; the
// newer tokens are copied to the other side. If Codex has since signed in
// elsewhere, the account is no longer shared. It must be called with the
// accounts lock held, and reports whether f changed.
func (e *env) reconcileCodex(f *store.File) (bool, error) {
	idx := slices.IndexFunc(f.Accounts, func(a store.Account) bool { return a.InCodex })
	if idx < 0 {
		return false, nil
	}
	acct := &f.Accounts[idx]
	cur, err := e.codex.Read()
	if err != nil {
		// Codex rewrites auth.json in place, so a read can catch it mid-write.
		// Try again next time rather than guess.
		return false, nil
	}
	var codexAcct store.Account
	if cur != nil && cur.IsChatGPT() {
		codexAcct, err = codexAccount(cur)
	}
	if cur == nil || !cur.IsChatGPT() || err != nil || codexAcct.Key() != acct.Key() {
		acct.InCodex = false // Codex signed out or signed in elsewhere
		return true, nil
	}

	switch {
	case codexAcct.Tokens == acct.Tokens:
		return false, nil
	case newer(codexAcct.Tokens, codexAcct.LastRefresh, acct.Tokens, acct.LastRefresh):
		acct.Tokens = codexAcct.Tokens
		if !codexAcct.LastRefresh.IsZero() {
			acct.LastRefresh = codexAcct.LastRefresh
		}
		acct.PlanType = codexAcct.PlanType
		acct.NeedsRelogin = false
		return true, nil
	default:
		if err := e.codex.UpdateTokens(cur, acct.Tokens, acct.LastRefresh); err != nil {
			return false, fmt.Errorf("updating Codex's sign-in for %s: %w", acct.Label, err)
		}
		return false, nil
	}
}

// switchResult describes what switchTo did.
type switchResult struct {
	account  store.Account  // now used by Codex
	previous *store.Account // what Codex used before, if it was a ChatGPT account
	imported bool           // previous wasn't stored and was added
	already  bool           // Codex already used account
	backup   string         // where a non-ChatGPT sign-in was saved
}

// switchTo hands the account that pick selects to Codex. The account Codex
// used before is kept (and added if it wasn't stored) so nothing is lost.
func (e *env) switchTo(ctx context.Context, pick func(*store.File) (int, error), force bool) (*switchResult, error) {
	mode, err := e.codex.StoreMode()
	if err != nil {
		return nil, err
	}
	if mode != "file" {
		return nil, fmt.Errorf("Codex keeps its sign-in in the OS keyring (cli_auth_credentials_store = %q in %s); switching only works with file storage", mode, filepath.Join(e.codex.Dir, "config.toml"))
	}

	unlock, err := e.store.Lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	f, err := e.store.Load()
	if err != nil {
		return nil, err
	}
	if _, err := e.reconcileCodex(f); err != nil {
		return nil, err
	}
	i, err := pick(f)
	if err != nil {
		return nil, err
	}
	if f.Accounts[i].NeedsRelogin {
		return nil, fmt.Errorf("%s needs to sign in again first: run `codex-usage login`", f.Accounts[i].Label)
	}
	targetKey := f.Accounts[i].Key()
	res := &switchResult{}

	cur, readErr := e.codex.Read()
	switch {
	case readErr != nil || (cur != nil && !cur.IsChatGPT()):
		if !force {
			reason := "Codex is signed in with an API key or another method"
			if readErr != nil {
				reason = fmt.Sprintf("Codex's sign-in couldn't be read (%v)", readErr)
			}
			return nil, fmt.Errorf("%s; switching replaces it. Pass --force to switch anyway (the current sign-in is backed up first)", reason)
		}
		res.backup = filepath.Join(filepath.Dir(e.store.Path), "codex-auth.backup.json")
		if err := e.codex.Backup(res.backup); err != nil {
			return nil, err
		}
	case cur != nil:
		prev, err := codexAccount(cur)
		if err != nil {
			return nil, err
		}
		if prev.Key() == targetKey {
			target := &f.Accounts[i]
			if !target.InCodex {
				// Codex signed in to this account on its own. Its session is the
				// one in use, so share that one from now on.
				if newer(prev.Tokens, prev.LastRefresh, target.Tokens, target.LastRefresh) {
					target.Tokens, target.LastRefresh = prev.Tokens, prev.LastRefresh
				}
				target.InCodex = true
			}
			if err := e.store.Save(f); err != nil {
				return nil, err
			}
			return &switchResult{account: *target, already: true}, nil
		}
		if j := f.Index(prev.Key()); j >= 0 {
			// reconcileCodex already synced a shared session. A separate one
			// is only worth keeping if ours no longer works.
			stored := &f.Accounts[j]
			if !stored.InCodex && stored.NeedsRelogin {
				stored.Tokens, stored.LastRefresh, stored.NeedsRelogin = prev.Tokens, prev.LastRefresh, false
			}
			p := *stored
			res.previous = &p
		} else {
			// Keep Codex's current account so you can switch back to it.
			prev.AddedAt = e.Now().UTC()
			saved := f.Upsert(prev, "")
			res.previous, res.imported = &saved, true
		}
	}

	i = f.Index(targetKey)
	target := &f.Accounts[i]
	if err := e.codex.SignIn(target.Tokens, target.AccountID, target.LastRefresh); err != nil {
		return nil, err
	}
	for j := range f.Accounts {
		f.Accounts[j].InCodex = j == i
	}
	if err := e.store.Save(f); err != nil {
		return nil, fmt.Errorf("Codex now uses %s, but saving accounts failed: %w", target.Label, err)
	}
	res.account = *target
	return res, nil
}

func (a *App) runSwitch(ctx context.Context, args []string) error {
	fs, config := a.newFlags("switch", "switch [<label|email>] [--force]")
	force := fs.Bool("force", false, "replace a sign-in that isn't a ChatGPT account (it's backed up first)")
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
		return e.printCodexAccount()
	}

	res, err := e.switchTo(ctx, func(f *store.File) (int, error) { return resolveAccount(f, positional[0]) }, *force)
	if err != nil {
		return err
	}
	e.reportSwitch(res, "")
	return nil
}

// who names an account with its email and plan: "work (work@acme.com, pro)".
func who(a store.Account) string {
	var details []string
	if a.Email != "" && a.Email != a.Label {
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

func (e *env) reportSwitch(res *switchResult, detail string) {
	st := e.styles()
	if res.already {
		fmt.Fprintf(e.Stdout, "Codex is already using %s%s.\n", who(res.account), detail)
		return
	}
	if res.backup != "" {
		fmt.Fprintf(e.Stdout, "Backed up Codex's previous sign-in to %s.\n", res.backup)
	}
	if res.imported {
		fmt.Fprintf(e.Stdout, "Saved Codex's previous account as %s.\n", st.bold(who(*res.previous)))
	}
	from := ""
	if res.previous != nil {
		from = " from " + res.previous.Label
	}
	fmt.Fprintf(e.Stdout, "%s Switched Codex%s to %s%s.\n", st.good("✓"), from, who(res.account), detail)
	fmt.Fprintln(e.Stdout, st.dim(restartNote))
}

func (e *env) printCodexAccount() error {
	cur, err := e.codex.Read()
	if err != nil {
		return err
	}
	if cur == nil {
		fmt.Fprintln(e.Stdout, "Codex isn't signed in.")
		return nil
	}
	if !cur.IsChatGPT() {
		fmt.Fprintln(e.Stdout, "Codex is signed in with an API key or another method, not a ChatGPT account.")
		return nil
	}
	acct, err := codexAccount(cur)
	if err != nil {
		return err
	}
	f, err := e.store.Load()
	if err != nil {
		return err
	}
	if i := f.Index(acct.Key()); i >= 0 {
		fmt.Fprintf(e.Stdout, "Codex is using %s.\n", who(f.Accounts[i]))
	} else {
		fmt.Fprintf(e.Stdout, "Codex is using %s, which isn't one of your codex-usage accounts. Switching to another account saves it.\n", describe(acct))
	}
	return nil
}

// candidate is an account rotate could switch to.
type candidate struct {
	result
	left   float64 // share left in the account's tightest window
	window string
}

func (a *App) runRotate(ctx context.Context, args []string) error {
	fs, config := a.newFlags("rotate", "rotate [--dry-run] [--force]")
	dryRun := fs.Bool("dry-run", false, "show which account would be used without switching")
	force := fs.Bool("force", false, "replace a sign-in that isn't a ChatGPT account (it's backed up first)")
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
	results, err := e.collect(ctx, nil)
	if err != nil {
		return err
	}
	if len(results) == 0 {
		return errors.New("no accounts yet; run `codex-usage login` to add one")
	}

	var candidates []candidate
	for _, r := range results {
		if c, ok := rotationCandidate(r); ok {
			candidates = append(candidates, c)
		}
	}
	slices.SortStableFunc(candidates, func(x, y candidate) int {
		switch {
		case x.left > y.left:
			return -1
		case x.left < y.left:
			return 1
		}
		return 0
	})

	current := e.codexKey()
	if len(candidates) == 0 {
		msg := "no account has usage left right now"
		var withResets []string
		for _, r := range results {
			if r.err == nil && resetCount(r) > 0 {
				withResets = append(withResets, r.account.Label)
			}
		}
		if len(withResets) > 0 {
			msg += fmt.Sprintf("; %s can use a usage limit reset: `codex-usage redeem %s`", strings.Join(withResets, ", "), withResets[0])
		}
		return errors.New(msg)
	}
	best := candidates[0]
	detail := fmt.Sprintf(": %.0f%% left in the %s window", best.left, best.window)
	if best.account.Key() == current {
		fmt.Fprintf(e.Stdout, "Codex is already using the account with the most usage left, %s%s.\n", who(best.account), detail)
		return nil
	}
	if *dryRun {
		fmt.Fprintf(e.Stdout, "Would switch Codex to %s%s.\n", who(best.account), detail)
		return nil
	}
	res, err := e.switchTo(ctx, func(f *store.File) (int, error) {
		if i := f.Index(best.account.Key()); i >= 0 {
			return i, nil
		}
		return 0, fmt.Errorf("%s was removed while rotating", best.account.Label)
	}, *force)
	if err != nil {
		return err
	}
	e.reportSwitch(res, detail)
	return nil
}

// rotationCandidate scores an account by the share left in its tightest
// window. Accounts that are blocked, failed or report no windows don't qualify.
func rotationCandidate(r result) (candidate, bool) {
	if r.err != nil || r.usage == nil || r.usage.RateLimit.Blocked() {
		return candidate{}, false
	}
	if r.usage.SpendControl != nil && r.usage.SpendControl.Reached {
		return candidate{}, false
	}
	windows := r.usage.RateLimit.Windows()
	if len(windows) == 0 {
		return candidate{}, false
	}
	c := candidate{result: r, left: 101}
	for _, w := range windows {
		if left := w.LeftPercent(); left < c.left {
			c.left, c.window = left, w.Label()
		}
	}
	if c.left <= 0 {
		return candidate{}, false
	}
	return c, true
}
