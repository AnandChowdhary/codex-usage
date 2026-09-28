package cli

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
	"github.com/AnandChowdhary/codex-usage/internal/store"
	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

const (
	noAccountsMessage = "No accounts yet. Run `codex-usage login` to add one."

	// Refresh an access token this long before it expires.
	refreshMargin = 5 * time.Minute
	// Refresh tokens without a readable expiry after this long, like Codex does.
	fallbackRefreshAge = 8 * 24 * time.Hour
)

// reloginError means an account's sign-in can no longer be refreshed.
type reloginError struct{ cause error }

func (e *reloginError) Error() string {
	reason := "signed out"
	if e.cause != nil {
		reason = e.cause.Error()
	}
	return reason + "; run `codex-usage login` to sign in again"
}

// result is the outcome of checking one account.
type result struct {
	account store.Account
	usage   *usage.Response
	resets  *usage.ResetCredits
	err     error
	warning error
}

func (a *App) runUsage(ctx context.Context, args []string) error {
	fs, config := a.newFlags("usage", "[usage] [--json] [--all]")
	asJSON := fs.Bool("json", false, "print JSON")
	all := fs.Bool("all", false, "also show per-model and other additional limits")
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

	results, err := e.collect(ctx)
	if err != nil {
		return err
	}
	if len(results) == 0 && !*asJSON {
		fmt.Fprintln(e.Stderr, noAccountsMessage)
		return nil
	}
	if *asJSON {
		if err := writeJSON(e.Stdout, e.usageJSON(results)); err != nil {
			return err
		}
	} else {
		e.renderUsage(results, *all)
	}
	for _, r := range results {
		if r.err != nil {
			return exitError(1)
		}
	}
	return nil
}

// collect refreshes stale sign-ins and fetches usage for every account.
func (e *env) collect(ctx context.Context) ([]result, error) {
	f, err := e.store.Load()
	if err != nil {
		return nil, err
	}
	now := e.Now()
	stale := map[string]string{}
	for _, acct := range f.Accounts {
		if !acct.NeedsRelogin && needsRefresh(acct, now) {
			stale[acct.Key()] = acct.Tokens.AccessToken
		}
	}
	var refreshErrs map[string]error
	if len(stale) > 0 {
		if f, refreshErrs, err = e.refresh(ctx, stale, false); err != nil {
			return nil, err
		}
	}

	results := make([]result, len(f.Accounts))
	var todo []int
	for i, acct := range f.Accounts {
		results[i].account = acct
		if acct.NeedsRelogin {
			results[i].err = &reloginError{cause: refreshErrs[acct.Key()]}
			continue
		}
		results[i].warning = refreshErrs[acct.Key()]
		todo = append(todo, i)
	}
	e.fetch(ctx, results, todo)

	// A rejected token might have been revoked early; refresh once and retry.
	retry := map[string]string{}
	for _, i := range todo {
		r := results[i]
		if errors.Is(r.err, usage.ErrUnauthorized) && r.warning == nil {
			retry[r.account.Key()] = r.account.Tokens.AccessToken
		}
	}
	if len(retry) > 0 {
		f, retryErrs, err := e.refresh(ctx, retry, true)
		if err != nil {
			return nil, err
		}
		var again []int
		for i := range results {
			key := results[i].account.Key()
			if _, ok := retry[key]; !ok {
				continue
			}
			idx := f.Index(key)
			if idx < 0 {
				results[i].err = errors.New("account was removed while checking usage")
				continue
			}
			results[i].account = f.Accounts[idx]
			switch {
			case f.Accounts[idx].NeedsRelogin:
				results[i].err = &reloginError{cause: retryErrs[key]}
			case retryErrs[key] != nil:
				results[i].err = retryErrs[key]
			default:
				again = append(again, i)
			}
		}
		e.fetch(ctx, results, again)
	}

	for i := range results {
		r := &results[i]
		if errors.Is(r.err, usage.ErrUnauthorized) {
			if r.warning != nil {
				r.err, r.warning = r.warning, nil
			} else {
				r.err = errors.New("ChatGPT rejected this sign-in; run `codex-usage login` to sign in again")
			}
		}
	}
	return results, nil
}

// fetch loads usage for results[i] for each i in idx, concurrently.
func (e *env) fetch(ctx context.Context, results []result, idx []int) {
	var wg sync.WaitGroup
	for _, i := range idx {
		wg.Add(1)
		go func(r *result) {
			defer wg.Done()
			creds := usage.Credentials{
				AccessToken: r.account.Tokens.AccessToken,
				AccountID:   r.account.AccountID,
				FedRAMP:     r.account.FedRAMP,
			}
			r.usage, r.err = e.usage.Fetch(ctx, creds)
			if r.err != nil || r.usage.AvailableResets() == 0 {
				return
			}
			// Usage only carries the count; the list adds expiry dates. It's
			// extra detail, so if it fails the count alone is shown.
			r.resets, _ = e.usage.FetchResetCredits(ctx, creds)
		}(&results[i])
	}
	wg.Wait()
}

// refresh renews the sign-ins in targets (account key → access token the
// caller saw) while holding the accounts lock, saving after each one because
// refresh tokens rotate. Without force, accounts another process refreshed
// meanwhile are skipped; with force, accounts whose access token changed are.
// It returns the updated file and per-account refresh errors.
func (e *env) refresh(ctx context.Context, targets map[string]string, force bool) (*store.File, map[string]error, error) {
	unlock, err := e.store.Lock(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer unlock()
	f, err := e.store.Load()
	if err != nil {
		return nil, nil, err
	}

	now := e.Now()
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		errs    = map[string]error{}
		saveErr error
	)
	for i, acct := range f.Accounts {
		seen, ok := targets[acct.Key()]
		if !ok || acct.NeedsRelogin {
			continue
		}
		if force && acct.Tokens.AccessToken != seen {
			continue
		}
		if !force && !needsRefresh(acct, now) {
			continue
		}
		wg.Add(1)
		go func(i int, key, refreshToken string) {
			defer wg.Done()
			var tokens *auth.Tokens
			var err error
			if refreshToken == "" {
				err = errors.New("no refresh token stored")
			} else {
				tokens, err = e.auth.Refresh(ctx, refreshToken)
			}

			mu.Lock()
			defer mu.Unlock()
			acct := &f.Accounts[i]
			var refreshErr *auth.RefreshError
			switch {
			case err == nil:
				applyRefresh(acct, tokens, e.Now().UTC())
			case refreshToken == "" || errors.As(err, &refreshErr) && refreshErr.Permanent:
				acct.NeedsRelogin = true
				errs[key] = err
			default:
				errs[key] = err
				return
			}
			if err := e.store.Save(f); err != nil && saveErr == nil {
				saveErr = err
			}
		}(i, acct.Key(), acct.Tokens.RefreshToken)
	}
	wg.Wait()
	return f, errs, saveErr
}

func needsRefresh(acct store.Account, now time.Time) bool {
	if exp, ok := auth.TokenExpiry(acct.Tokens.AccessToken); ok {
		return !exp.After(now.Add(refreshMargin))
	}
	return acct.LastRefresh.Before(now.Add(-fallbackRefreshAge))
}

func applyRefresh(acct *store.Account, tokens *auth.Tokens, now time.Time) {
	if tokens.IDToken != "" {
		acct.Tokens.IDToken = tokens.IDToken
		if id, err := auth.ParseIdentity(tokens.IDToken); err == nil {
			acct.ApplyIdentity(id)
		}
	}
	if tokens.AccessToken != "" {
		acct.Tokens.AccessToken = tokens.AccessToken
	}
	if tokens.RefreshToken != "" {
		acct.Tokens.RefreshToken = tokens.RefreshToken
	}
	acct.LastRefresh = now
}
