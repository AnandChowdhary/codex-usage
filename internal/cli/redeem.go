package cli

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/store"
	"github.com/AnandChowdhary/codex-usage/internal/usage"
)

const redeemAttempts = 3

// redeemRetryDelay is the pause before retry n is n times this long.
var redeemRetryDelay = time.Second

func (a *App) runRedeem(ctx context.Context, args []string) error {
	fs, config := a.newFlags("redeem", "redeem <label|email> [--credit ID] [--yes]")
	creditFlag := fs.String("credit", "", "ID of the reset to use (default: the one that expires first)")
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

	f, err := e.store.Load()
	if err != nil {
		return err
	}
	i, err := resolveAccount(f, positional[0])
	if err != nil {
		return err
	}
	key := f.Accounts[i].Key()
	results, err := e.collect(ctx, func(acct store.Account) bool { return acct.Key() == key })
	if err != nil {
		return err
	}
	if len(results) != 1 {
		return errors.New("the account was removed while checking it")
	}
	r := results[0]
	name := r.account.Label
	if r.err != nil {
		return fmt.Errorf("%s: %w", name, r.err)
	}
	count := resetCount(r)
	if count == 0 {
		return fmt.Errorf("%s has no usage limit resets available", name)
	}
	credit, err := pickResetCredit(r.resets, *creditFlag)
	if err != nil {
		return err
	}

	st := e.styles()
	now := e.Now()
	fmt.Fprintln(e.Stdout, st.bold(describe(r.account)))
	e.printLimits(r.usage.RateLimit, now)
	fmt.Fprintf(e.Stdout, "  %s\n\n", e.resetNote(count, r.resets, now))

	if !*yes {
		fmt.Fprintf(e.Stdout, "Use a usage limit reset on %s? This clears its current usage limits and can't be undone. [y/N] ", name)
		if !confirm(e.Stdin, e.Stdout) {
			fmt.Fprintln(e.Stdout, "Cancelled; nothing was redeemed.")
			return nil
		}
	}

	creds := usage.Credentials{AccessToken: r.account.Tokens.AccessToken, AccountID: r.account.AccountID, FedRAMP: r.account.FedRAMP}
	res, err := e.redeem(ctx, creds, credit)
	if err != nil {
		return err
	}
	switch res.Code {
	case usage.RedeemReset, usage.RedeemAlreadyRedeemed:
		msg := "Usage limits reset for " + name
		if n := res.WindowsReset; n == 1 {
			msg += " (1 window)"
		} else if n > 1 {
			msg += fmt.Sprintf(" (%d windows)", n)
		}
		fmt.Fprintf(e.Stdout, "%s %s.\n", st.good("✓"), msg)
		if after, err := e.usage.Fetch(ctx, creds); err == nil {
			e.printLimits(after.RateLimit, e.Now())
		}
		return nil
	case usage.RedeemNothingToReset:
		return fmt.Errorf("%s's usage doesn't need a reset right now", name)
	case usage.RedeemNoCredit:
		if credit != "" {
			return errors.New("that reset is no longer available; run `codex-usage` to see current resets")
		}
		return fmt.Errorf("%s has no usage limit resets available", name)
	default:
		return fmt.Errorf("unexpected response from ChatGPT: %q", res.Code)
	}
}

// redeem spends a reset, retrying transient failures with the same request
// ID so the server never spends two.
func (e *env) redeem(ctx context.Context, creds usage.Credentials, creditID string) (*usage.RedeemResult, error) {
	requestID := newRequestID()
	for attempt := 1; ; attempt++ {
		res, err := e.usage.Redeem(ctx, creds, requestID, creditID)
		if err == nil {
			return res, nil
		}
		if !redeemRetryable(err) {
			return nil, err
		}
		if attempt == redeemAttempts {
			return nil, fmt.Errorf("%w\nThe reset may or may not have been used; run `codex-usage` to check before trying again", err)
		}
		t := time.NewTimer(time.Duration(attempt) * redeemRetryDelay)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// redeemRetryable reports whether a failed redeem might succeed on retry.
// Network errors leave the outcome unknown; retrying with the same request
// ID is safe.
func redeemRetryable(err error) bool {
	if errors.Is(err, usage.ErrUnauthorized) || errors.Is(err, context.Canceled) {
		return false
	}
	var status *usage.StatusError
	if errors.As(err, &status) {
		return status.Status == 429 || status.Status >= 500
	}
	return true
}

// pickResetCredit chooses which reset to spend: the requested one, else the
// one that expires first. It returns "" when the list is unknown, letting the
// server pick.
func pickResetCredit(resets *usage.ResetCredits, requested string) (string, error) {
	if resets == nil {
		return requested, nil
	}
	available := resets.Available()
	if requested == "" {
		if len(available) == 0 {
			return "", nil
		}
		return available[0].ID, nil
	}
	for _, c := range available {
		if c.ID == requested {
			return requested, nil
		}
	}
	return "", fmt.Errorf("no available reset has ID %q; see `codex-usage --json`", requested)
}

func (e *env) printLimits(limit *usage.RateLimit, now time.Time) {
	for _, w := range limit.Windows() {
		fmt.Fprintf(e.Stdout, "  %-7s %3.0f%% left, resets %s\n", w.Label(), w.LeftPercent(), formatWhen(now, w.ResetTime(now), e.Location))
	}
}

// confirm reads a yes/no answer; anything but "y" or "yes" is no.
func confirm(in io.Reader, out io.Writer) bool {
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		fmt.Fprintln(out) // no newline was typed, e.g. stdin isn't a terminal
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// newRequestID returns a random UUID v4.
func newRequestID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
