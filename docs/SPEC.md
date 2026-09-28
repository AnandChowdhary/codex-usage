# codex-usage — spec

Status: v1 implemented · 2026-09-28

## Goal

Sign in to several ChatGPT accounts (each with a Codex-eligible plan) using
Codex's device-code login, store the credentials locally, and print the
remaining Codex usage for every account in one command.

```
$ codex-usage
ACCOUNT              PLAN   5H LEFT  RESETS IN  WEEKLY LEFT  RESETS        CREDITS
work@acme.com        pro    72%      2h13m      41%          Thu 09:00     -
me@gmail.com         plus   0% ⛔    38m        12%          Mon 14:20     4.20
side@proton.me       plus   -        -          -            -             re-login needed
```

## Non-goals (for now)

- Starting Codex sessions.
- API-key accounts. Usage limits only apply to ChatGPT-plan logins.
- OS keychain storage. Planned for v2 (see `TODO.md`); v1 matches Codex's
  plaintext `auth.json` model.

## Commands

| Command | Behaviour |
|---|---|
| `codex-usage` / `codex-usage usage` | Fetch usage for all stored accounts concurrently and print a table. `--json` prints raw, normalized JSON. Exits non-zero if any account failed. |
| `codex-usage login [--label NAME] [--open]` | Run the device-code flow, then add or update the account. The label defaults to the email. `--open` also launches the browser. |
| `codex-usage accounts` | List stored accounts (label, email, plan, workspace, last refresh). No network calls. |
| `codex-usage switch [<label\|email>] [--force]` | Sign the Codex CLI in to the account, or show which one it uses (see §5). |
| `codex-usage rotate [--dry-run] [--force]` | Switch Codex to the account with the most usage left (see §5). |
| `codex-usage claude login\|logout\|switch\|rotate\|run …` | Claude Code profiles (see §6). |
| `codex-usage redeem <label\|email> [--credit ID] [--yes]` | Use a usage limit reset after confirming (see §4). |
| `codex-usage logout <label\|email>` | Revoke the refresh token (best effort) and remove the account. |
| `codex-usage --version` | Print the version. |

Global flag: `--config PATH` (defaults below). Env overrides mirror Codex's
own and exist for tests:
`CODEX_USAGE_HOME`, `CODEX_USAGE_AUTH_BASE_URL`, `CODEX_USAGE_CHATGPT_BASE_URL`.

## Protocol

Everything below comes from the open-source Codex CLI
(`openai/codex`, commit `44fe510`, `codex-rs/login` + `codex-rs/backend-client`).
I verified the usercode and usage endpoints live on 2026-09-28. These are
**private, undocumented endpoints**, so they may change without notice.

Constants:

- Issuer: `https://auth.openai.com`
- Client ID: `app_EMoamEEZ73f0CkXaXp7hrann` (Codex CLI's public OAuth client)
- ChatGPT backend: `https://chatgpt.com/backend-api`

### 1. Device-code login

1. **Request a code**
   `POST {issuer}/api/accounts/deviceauth/usercode`
   JSON `{"client_id": CLIENT_ID}` →
   `{"device_auth_id", "user_code", "interval": "5", "expires_at"}`
   - `interval` is a **string** of seconds. Accept both a string and a number.
   - Also accept `usercode` as an alias for `user_code`.
   - A 404 means device login isn't enabled. Tell the user to check that
     device-code sign-in is allowed in ChatGPT's security settings, or by
     their workspace admin.
2. **Show the prompt**: open `{issuer}/codex/device` and enter `user_code`.
   The code expires in 15 minutes. Print the same "only continue if you
   started this" warning that Codex prints.
3. **Poll**
   `POST {issuer}/api/accounts/deviceauth/token`
   JSON `{"device_auth_id", "user_code"}` every `interval` seconds.
   - 403 / 404 → still pending, keep polling (give up after 15 minutes).
   - 200 → `{"authorization_code", "code_challenge", "code_verifier"}`.
   - Anything else → fail.
4. **Exchange**
   `POST {issuer}/oauth/token`, **form-encoded**:
   `grant_type=authorization_code`, `client_id`, `code=authorization_code`,
   `redirect_uri={issuer}/deviceauth/callback`, `code_verifier`
   → `{"id_token", "access_token", "refresh_token"}`.
   **Never retry this after a response timeout**, because the code is single-use.
5. **Identify the account**: base64-decode the `id_token` payload. No
   signature check is needed, since it's for display and keys only.
   - `email`
   - `["https://api.openai.com/auth"].chatgpt_plan_type`
   - `["https://api.openai.com/auth"].chatgpt_account_id` (workspace)
   - `["https://api.openai.com/auth"].chatgpt_user_id`
   - `["https://api.openai.com/auth"].chatgpt_account_is_fedramp`

### 2. Token refresh

- Refresh if the `access_token` JWT's `exp` falls within 5 minutes. If `exp`
  can't be parsed, refresh when `last_refresh` is more than 8 days old
  (Codex's rule).
- `POST {issuer}/oauth/token`, **JSON**:
  `{"grant_type":"refresh_token","client_id","refresh_token"}` →
  optional `id_token`, `access_token`, `refresh_token`.
- **Refresh tokens rotate.** Write the new tokens to disk before doing
  anything else.
- Permanent failures mark the account `needs_relogin` and skip it:
  - 401
  - 400 `invalid_grant`
  - error codes `refresh_token_expired`, `refresh_token_reused`, and
    `refresh_token_invalidated`

  Anything else is transient, so show the error and keep the account.

### 3. Usage

`GET {chatgpt}/wham/usage`

Headers:
- `Authorization: Bearer <access_token>`
- `ChatGPT-Account-Id: <chatgpt_account_id>`
- `User-Agent: codex-usage/<version>`
- `X-OpenAI-Fedramp: true` (only for FedRAMP accounts)

Use a cookie jar per run. Codex keeps Cloudflare cookies for chatgpt.com.

Response (fields we use):

```jsonc
{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window":   { "used_percent": 28, "limit_window_seconds": 18000,
                          "reset_after_seconds": 7980, "reset_at": 1790000000 },
    "secondary_window": { "used_percent": 59, "limit_window_seconds": 604800,
                          "reset_after_seconds": 250000, "reset_at": 1790250000 }
  },
  "credits": { "has_credits": true, "unlimited": false, "balance": "4.20" },
  "additional_rate_limits": [
    { "limit_name": "...", "metered_feature": "...", "rate_limit": { /* same shape */ } }
  ],
  "rate_limit_reached_type": { "type": "rate_limit_reached" }
}
```

- Every nested object may be `null` or missing, so decode defensively.
- **Observed live (Pro account, 2026-09-28):** `primary_window` was the
  *weekly* window (`limit_window_seconds: 604800`) and `secondary_window`
  was `null`. The response also carried `code_review_rate_limit` (same shape as
  `rate_limit`), `spend_control: {"reached": bool}`, `model_usage`, and
  `rate_limit_reset_credits`. So never assume primary means 5h.
- The table has one "LEFT / RESETS" column pair per distinct window length
  across all accounts, shortest first (e.g. `5H`, `WEEKLY`). Accounts without
  that window show `-`. Windows are labelled by `limit_window_seconds`:
  18000 → "5h", 86400 → "daily", 604800 → "weekly", and anything else is
  formatted as a duration.
- Show "left" as `100 - used_percent`.
- Show resets as relative time when under 24h, otherwise as a local weekday
  and time.
- `--all` adds rows for `code_review_rate_limit` and `additional_rate_limits`
  (e.g. per-model limits).
- Notes show when a limit or spend control is reached, and any credit balance.

### 4. Usage limit resets

Accounts can earn "usage limit resets". Redeeming one clears the current
limits; it's the "Redeem reset" item in Codex's `/usage` menu.

- `/wham/usage` includes a summary:
  `"rate_limit_reset_credits": {"available_count", "applicable_available_count"}`.
  Codex only reads `available_count`, so we do the same.
- If `available_count > 0`, also call
  `GET {chatgpt}/wham/rate-limit-reset-credits`. It uses the same headers as
  usage, and I verified it live with a device-code sign-in on 2026-09-28. It
  returns:
  `{"credits": [{"id", "reset_type", "status", "granted_at", "expires_at", "title", "description"}], "available_count", "total_earned_count", "immediate_reset_purchase_eligible", "history_enabled"}`.
  - `status` is `available`, `redeeming` or `redeemed`.
  - Timestamps are RFC 3339.
  - `expires_at` is `null` if the credit never expires.
  - `title` and `description` may be `null`. Codex falls back to "Full reset"
    and "Reset your current usage limits".
- Notes show `N usage limit reset(s) available (first expires …)`, using the
  available credit that expires soonest. If the list request fails, only the
  count is shown.
- **Redeeming** (`codex-usage redeem <account> [--credit ID] [--yes]`):
  `POST {chatgpt}/wham/rate-limit-reset-credits/consume`
  `{"redeem_request_id": <uuid v4>, "credit_id"?}` →
  `{"code": "reset" | "nothing_to_reset" | "no_credit" | "already_redeemed", "windows_reset": n}`.
  - `redeem_request_id` is an idempotency key. Retry network errors, 429 and
    5xx (up to 3 tries) with the **same** ID. If every try fails, say the
    outcome is unknown.
  - `reset` and `already_redeemed` (a retry of a request that went through)
    both mean success, as in Codex.
  - `nothing_to_reset` means usage doesn't need a reset. `no_credit` means
    the chosen reset (or any reset) isn't available.
  - Default to the available reset that expires first. Always ask for
    confirmation unless `--yes` is passed. Show usage again afterwards.

### 5. Switching the Codex CLI's account

`switch <account>` and `rotate` hand an account's session to the Codex CLI.
This is based on how Codex stores and reloads its sign-in (`codex-rs/login`,
`storage.rs` and `manager.rs`):

- Codex keeps its sign-in in `$CODEX_HOME/auth.json` (default `~/.codex`)
  unless `cli_auth_credentials_store` in `config.toml` says `keyring`,
  `auto` or `ephemeral`. We only support file storage and refuse otherwise.
  Only top-level keys are checked.
- `codex login` writes:
  `{"auth_mode": "chatgpt", "OPENAI_API_KEY": null, "tokens": {"id_token", "access_token", "refresh_token", "account_id"}, "last_refresh"}`.
  We write the same, atomically, with mode `0600`. Without `auth_mode`, a
  file with `OPENAI_API_KEY` means API-key mode.
- **Before every refresh, Codex rereads auth.json.**
  - Same account, changed tokens: Codex adopts them and skips its own refresh.
  - Different account: the running process refuses to refresh ("signed in to
    another account"). That's why running sessions must be restarted after a
    switch.
  - Nothing watches the file, so a running session keeps its cached access
    token until then.

**Shared session.** The switched-to account is marked `in_codex`. It shares
one session (one refresh-token family) with `auth.json`. Refresh tokens
rotate, so the two copies must never diverge:

- Every run reconciles them under the accounts lock, before and after
  refreshing. The copy whose access token expires later wins (falling back
  to `last_refresh`), and it's written to the other side. When we push to
  `auth.json`, fields we don't know about are kept.
- If `auth.json` can't be parsed (Codex rewrites it in place), we skip that
  run instead of guessing.
- If `auth.json` holds a different account, or is gone, the flag is cleared.
  Our copy may be stale in that case; a later refresh then reports
  `refresh_token_reused` and the account asks to sign in again.

**Replacing Codex's sign-in.**
- Codex's current account is reconciled first.
- If Codex is on an account we don't have, it's added (so switching back
  works).
- Any other kind of sign-in (API key, unreadable file) needs `--force`, and
  the current file is copied to `~/.codex-usage/codex-auth.backup.json`
  first.
- If Codex is already on the target account through its own `codex login`,
  we adopt whichever session is newer.

`logout` never revokes the session of the account Codex is using, because
that would sign Codex out too.

**rotate.**
- Score every account that was checked successfully, isn't blocked or at its
  spend limit, and reports at least one window. The score is `min(left
  percent)` across its windows.
- Switch to the highest score. Ties keep the stored order.
- If none qualify, fail and name the accounts that have usage limit resets.
- The score is a percentage, so a Pro and a Plus plan with the same
  percentage left rank equally, even though Pro's limits are larger.

### 6. Claude Code profiles

Researched in Claude Code 2.1.282 and tested live with two Max accounts on
2026-09-28.

**Why this is different from Codex.** Claude Code's OAuth works much like
Codex's: PKCE with a pasted code, rotating refresh tokens, and a private usage
endpoint. But
[Anthropic's terms](https://code.claude.com/docs/en/legal-and-compliance#authentication-and-credential-use)
say third-party developers may not "offer Claude.ai login into their own
applications" or "collect, store, or intermediate Claude.ai credentials or
session tokens". So codex-usage never handles Claude tokens: every operation
runs the unmodified `claude` binary.

- **Profiles.** A profile is a directory used as `CLAUDE_CONFIG_DIR`,
  `$CODEX_USAGE_HOME/claude/<profile>`. Names match
  `[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}`, and `default` means Claude Code's own
  sign-in (no `CLAUDE_CONFIG_DIR`).
  - Claude Code keys its macOS Keychain entry on the directory path, so paths
    are made absolute and profiles are never renamed.
- **Sign-in** is `claude auth login --claudeai`, run interactively with the
  profile's environment. It prints a claude.com link, and on a machine
  without a browser it asks for the code.
- **Status** is `claude auth status`, which prints JSON with `loggedIn`,
  `authMethod` (`claude.ai` for plans), `email`, `orgName` and
  `subscriptionType`. It exits 1 when signed out, but still prints JSON.
- **Usage** comes from
  `claude -p /usage --model haiku --tools "" --strict-mcp-config --no-session-persistence --output-format json`,
  run from an empty temp dir with `TZ=UTC`.
  - Claude Code answers `/usage` locally: `local_command: "usage"`,
    `num_turns: 0`, `total_cost_usd: 0`. If that ever changes, the table gets
    a note.
  - `result` holds text like:

    ```
    Current session: 4% used · resets Sep 28, 5:59pm (UTC)
    Current week (all models): 27% used · resets Sep 29, 9:59pm (UTC)
    Current week (Fable): 0% used · resets Sep 29, 10pm (UTC)
    ```

  - "session" is the 5-hour window and "all models" is the weekly window.
    Other named weeks are per-model limits.
  - Reset times have no year, so the next matching date is used. With no
    session in progress there's no reset time at all.
  - Lines we don't recognise are shown as notes.
  - A headless "hi" also returns these windows, in `rate_limit_event.unifiedWindows`.
    But those fields are undocumented, and the request costs about 6.7k
    tokens and starts a 5-hour window, so `/usage` is used instead.
- **Environment.** For login, status, usage and logout, `CLAUDE_CONFIG_DIR`,
  `CLAUDECODE`, `ANTHROPIC_*` and `CLAUDE_CODE_*` are removed. Otherwise an
  API key, gateway or the parent Claude session could take over. `claude run`
  keeps the caller's environment apart from `CLAUDE_CONFIG_DIR`.
- **Switching** can't change another process's environment. `claude switch`
  therefore:
  - writes `claude/current` (the profile name), plus `current.sh` and
    `current.fish` for shells to source at startup;
  - prints `export CLAUDE_CONFIG_DIR=…` (or `unset`) for `eval`;
  - prints its messages to stderr.

  If `CODEX_USAGE_CLAUDE_PROFILE` (set by `current.sh`) is missing, it also
  suggests the startup line to add.
- **rotate** uses the same score as Codex: the lowest share left across the
  5-hour and weekly windows. Per-model windows are ignored, and profiles at a
  limit are skipped.
- **Logout** runs `claude auth logout`, then deletes the directory after a
  confirmation. If it was the current profile, new shells go back to
  `default`.

## Storage

`$CODEX_USAGE_HOME` or `~/.codex-usage/accounts.json`. The directory is
`0700` and the file is `0600`. Writes are atomic (write a temp file, fsync,
then rename). A file lock is taken during refreshes so that two concurrent
runs can't both spend the same refresh token.

```jsonc
{
  "version": 1,
  "accounts": [
    {
      "label": "work",
      "email": "work@acme.com",
      "plan_type": "pro",
      "chatgpt_user_id": "user-…",
      "chatgpt_account_id": "…",
      "fedramp": false,
      "tokens": { "id_token": "…", "access_token": "…", "refresh_token": "…" },
      "last_refresh": "2026-09-28T10:00:00Z",
      "added_at": "2026-09-28T10:00:00Z",
      "needs_relogin": false
    }
  ]
}
```

An account's identity is `(chatgpt_user_id, chatgpt_account_id)`. One login
can reach several workspaces (e.g. personal Plus and a Team), and each one is
its own row. Logging in again to an existing identity replaces its tokens and
keeps its label.

## Code layout

Standard library only (`net/http`, `encoding/json`, `flag`, `text/tabwriter`).

```
main.go                  entry; calls cli.Run
internal/cli/            subcommand dispatch, table/JSON rendering
internal/auth/           device flow, code exchange, refresh, JWT claim decoding
internal/store/          accounts.json load/save, locking, atomic write
internal/usage/          /wham/usage client + response types + window labeling
```

## Testing

- Unit tests use `httptest` fakes for every endpoint: pending → success
  polling, 404 usercode, rotating refresh, each permanent refresh error,
  and null-heavy usage payloads.
- Store tests cover permissions, atomic replace, dedup and upsert, and
  concurrent-refresh locking.
- A manual end-to-end test logs in with two real accounts and compares the
  output to Codex's `/status`.

## Milestones

1. `store` + JWT decoding + `accounts` command.
2. `login` (device flow + exchange + upsert).
3. Refresh + `usage` table (concurrent, per-account errors).
4. `--json`, `--all`, `logout` with revoke, colors/TTY detection, README.

## Risks / open questions

- **Unofficial API.** It reuses Codex's OAuth client and private endpoints.
  It could break or be restricted at any time, and it's worth checking
  OpenAI's terms. It only ever reads usage for the user's own accounts.
- **Device login may need enabling** per account or workspace. Surface a
  clear error for the 404.
- **Cloudflare** could start challenging non-browser clients on chatgpt.com.
  An unauthenticated request currently gets a clean 401, which is fine.
- ~~Should `login` open the browser automatically?~~ Decided: print the URL
  by default; `--open` also launches the browser.
