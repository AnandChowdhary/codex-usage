package cli

import (
	"context"
	"fmt"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
)

func (a *App) runLogin(ctx context.Context, args []string) error {
	fs, config := a.newFlags("login", "login [--label NAME] [--open]")
	label := fs.String("label", "", "name for this account (default: its email)")
	open := fs.Bool("open", false, "open the sign-in page in a browser")
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

	dc, err := e.auth.RequestDeviceCode(ctx)
	if err != nil {
		return err
	}
	st := e.styles()
	fmt.Fprintf(e.Stdout, "Sign in to a ChatGPT account with Codex access.\n\n")
	fmt.Fprintf(e.Stdout, "1. Open this link and sign in:\n   %s\n\n", st.accent(dc.VerificationURL))
	fmt.Fprintf(e.Stdout, "2. Enter this one-time code %s:\n   %s\n\n", st.dim("(expires in 15 minutes)"), st.accent(dc.UserCode))
	fmt.Fprintln(e.Stdout, st.dim("Only continue if you started this sign-in. If a website or another person gave you this code, cancel."))
	fmt.Fprintln(e.Stdout)
	if *open {
		if err := e.OpenBrowser(dc.VerificationURL); err != nil {
			fmt.Fprintf(e.Stderr, "Couldn't open a browser (%v); open the link above instead.\n", err)
		}
	}
	fmt.Fprintln(e.Stdout, "Waiting for approval…")

	tokens, err := e.auth.CompleteDeviceLogin(ctx, dc)
	if err != nil {
		return err
	}
	saved, err := e.saveLogin(ctx, tokens, *label)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "\n%s Signed in as %s. Saved as %s.\n", st.good("✓"), describe(saved), st.bold(saved.Label))
	return nil
}

func (e *env) saveLogin(ctx context.Context, tokens *auth.Tokens, label string) (store.Account, error) {
	id, err := auth.ParseIdentity(tokens.IDToken)
	if err != nil {
		return store.Account{}, fmt.Errorf("reading ID token: %w", err)
	}
	now := e.Now().UTC()
	acct := store.Account{Tokens: *tokens, LastRefresh: now, AddedAt: now}
	acct.ApplyIdentity(id)

	unlock, err := e.store.Lock(ctx)
	if err != nil {
		return store.Account{}, err
	}
	defer unlock()
	f, err := e.store.Load()
	if err != nil {
		return store.Account{}, err
	}
	saved := f.Upsert(acct, label)
	if err := e.store.Save(f); err != nil {
		return store.Account{}, err
	}
	return saved, nil
}

func describe(a store.Account) string {
	name := a.Email
	if name == "" {
		name = a.Label
	}
	if a.PlanType != "" {
		name += " (" + a.PlanType + ")"
	}
	return name
}

func (a *App) runAccounts(args []string) error {
	fs, config := a.newFlags("accounts", "accounts [--json]")
	asJSON := fs.Bool("json", false, "print JSON")
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
	f, err := e.store.Load()
	if err != nil {
		return err
	}

	if *asJSON {
		type account struct {
			Label        string `json:"label"`
			Email        string `json:"email,omitempty"`
			Plan         string `json:"plan,omitempty"`
			UserID       string `json:"chatgpt_user_id,omitempty"`
			AccountID    string `json:"chatgpt_account_id,omitempty"`
			LastRefresh  string `json:"last_refresh,omitempty"`
			NeedsRelogin bool   `json:"needs_relogin"`
		}
		out := struct {
			Accounts []account `json:"accounts"`
		}{Accounts: []account{}}
		for _, acct := range f.Accounts {
			out.Accounts = append(out.Accounts, account{
				Label:        acct.Label,
				Email:        acct.Email,
				Plan:         acct.PlanType,
				UserID:       acct.UserID,
				AccountID:    acct.AccountID,
				LastRefresh:  formatRFC3339(acct.LastRefresh),
				NeedsRelogin: acct.NeedsRelogin,
			})
		}
		return writeJSON(e.Stdout, out)
	}

	if len(f.Accounts) == 0 {
		fmt.Fprintln(e.Stderr, noAccountsMessage)
		return nil
	}
	st := e.styles()
	t := &table{header: []string{"LABEL", "EMAIL", "PLAN", "WORKSPACE", "LAST REFRESH", "STATUS"}}
	now := e.Now()
	for _, acct := range f.Accounts {
		status := cell{text: "ok", style: st.good}
		if acct.NeedsRelogin {
			status = cell{text: "sign in again", style: st.bad}
		}
		t.rows = append(t.rows, []cell{
			{text: acct.Label},
			{text: orDash(acct.Email)},
			{text: orDash(acct.PlanType)},
			{text: orDash(shortID(acct.AccountID)), style: st.dim},
			{text: formatAgo(now, acct.LastRefresh)},
			status,
		})
	}
	t.write(e.Stdout, st)
	return nil
}

func (a *App) runLogout(ctx context.Context, args []string) error {
	fs, config := a.newFlags("logout", "logout <label|email>")
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

	removed, err := e.removeAccount(ctx, positional[0])
	if err != nil {
		return err
	}
	if removed.Tokens.RefreshToken != "" {
		if err := e.auth.Revoke(ctx, removed.Tokens.RefreshToken); err != nil {
			fmt.Fprintf(e.Stderr, "Removed the account, but couldn't end its session on the server: %v\n", err)
		}
	}
	fmt.Fprintf(e.Stdout, "Signed out of %s.\n", describe(removed))
	return nil
}

func (e *env) removeAccount(ctx context.Context, query string) (store.Account, error) {
	unlock, err := e.store.Lock(ctx)
	if err != nil {
		return store.Account{}, err
	}
	defer unlock()
	f, err := e.store.Load()
	if err != nil {
		return store.Account{}, err
	}
	i, err := resolveAccount(f, query)
	if err != nil {
		return store.Account{}, err
	}
	removed := f.Accounts[i]
	f.Remove(i)
	if err := e.store.Save(f); err != nil {
		return store.Account{}, err
	}
	return removed, nil
}

// resolveAccount finds the one account a label or email refers to.
func resolveAccount(f *store.File, query string) (int, error) {
	matches := f.Find(query)
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("no account matches %q; see `codex-usage accounts`", query)
	case 1:
		return matches[0], nil
	default:
		var labels []string
		for _, i := range matches {
			labels = append(labels, f.Accounts[i].Label)
		}
		return 0, fmt.Errorf("%q matches several accounts (%s); pass a label instead", query, joinQuoted(labels))
	}
}
