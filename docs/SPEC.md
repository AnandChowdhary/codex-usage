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

- Starting Codex sessions, or switching the account the Codex CLI uses.
- Importing `~/.codex/auth.json`. Refresh tokens rotate and are single-use.
  If this tool and the Codex CLI shared one refresh token, whichever refreshed
  second would get `refresh_token_reused` and be logged out. Every account
  here gets its own device-code session.
- API-key accounts. Usage limits only apply to ChatGPT-plan logins.
- OS keychain storage. Planned for v2 (see `TODO.md`); v1 matches Codex's
  plaintext `auth.json` model.

## Commands

| Command | Behaviour |
|---|---|
| `codex-usage` / `codex-usage usage` | Fetch usage for all stored accounts concurrently and print a table. `--json` prints raw, normalized JSON. Exits non-zero if any account failed. |
| `codex-usage login [--label NAME] [--open]` | Run the device-code flow, then add or update the account. The label defaults to the email. `--open` also launches the browser. |
| `codex-usage accounts` | List stored accounts (label, email, plan, workspace, last refresh). No network calls. |
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
- Label windows by `limit_window_seconds`: 18000 → "5h", 604800 →
  "weekly", and anything else is formatted as a duration. Don't assume
  that primary always means 5h.
- Show "left" as `100 - used_percent`.
- Show resets as relative time when under 24h, otherwise as a local weekday
  and time.
- `additional_rate_limits` (e.g. per-model limits) appear as extra rows with
  `--all`.

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
