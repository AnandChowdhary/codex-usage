# codex-usage

Shows how much Codex usage is left across all your ChatGPT accounts.

```
$ codex-usage
ACCOUNT         PLAN  5H LEFT  RESETS  WEEKLY LEFT  RESETS       NOTES
work@acme.com   pro   -        -       81%          Mon 07:33
me@example.com  plus  0%       38m     12%          Oct 8 12:00  limit reached; credits: 4.20
```

There's one column pair for each limit window that your accounts report.
Plans differ: a Pro account may only have a weekly window, while Plus has both
5-hour and weekly windows.

## Install

```sh
go build -o codex-usage .   # Go 1.24+
```

## Use

```sh
codex-usage login                 # sign in with a device code; repeat for each account
codex-usage login --label work    # name the account (default: its email)
codex-usage login --open          # also open the sign-in page in a browser
codex-usage                       # usage for every account
codex-usage --all                 # include per-model and other extra limits
codex-usage --json                # machine-readable
codex-usage accounts              # list signed-in accounts
codex-usage logout work           # sign out and forget an account
```

`login` prints a link (`https://auth.openai.com/codex/device`) and a one-time
code. Open the link, sign in to the ChatGPT account you want to add, and enter
the code. If you see "device code sign-in is not enabled", turn on device-code
sign-in for Codex in ChatGPT's security settings, or ask your workspace admin
to enable it.

Each account and workspace pair is a separate entry. If one login has both a
personal plan and a Team workspace, sign in twice and pick a different
workspace each time.

`codex-usage` exits with status 1 if any account couldn't be checked, for
example because it needs to sign in again.

## How it works

Everything uses the same sign-in and endpoints as the Codex CLI:

- device-code login and token refresh go to `auth.openai.com`
- usage comes from `chatgpt.com/backend-api/wham/usage`

Every account gets its own sign-in session. This tool never reads or changes
`~/.codex/auth.json`, so your Codex CLI login isn't affected. See
[docs/SPEC.md](docs/SPEC.md) for the protocol details.

These are private OpenAI endpoints, so they can change without notice.

## Storage

Accounts are stored in `~/.codex-usage/accounts.json`. You can change the
location with `$CODEX_USAGE_HOME` or `--config PATH`.

The file contains sign-in tokens. It's only readable by you (mode `0600`) and
is written atomically. Keychain storage is planned for v2 (see
[TODO.md](TODO.md)).

## Develop

```sh
go test -race ./...
```

`CODEX_USAGE_AUTH_BASE_URL` and `CODEX_USAGE_CHATGPT_BASE_URL` point the CLI
at other servers.
