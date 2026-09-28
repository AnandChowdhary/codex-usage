# TODO

## v2

- [ ] **OS keychain storage.** Keep tokens in the macOS Keychain, Windows
      Credential Manager, or Secret Service (Linux) instead of
      `accounts.json`. Leave only non-secret metadata in the file, and fall back
      to the file where no keychain is available. v1 stores tokens in plaintext
      with mode `0600`, like Codex's own `auth.json`.

## Later

- [ ] `rename <label> <new-label>`, so a label can change without signing in again.
- [ ] `usage <label|email>…` to check only some accounts.
- [ ] `redeem <label|email>` to use an available usage limit reset, via
      `POST /wham/rate-limit-reset-credits/consume`. It spends a credit and
      can't be undone, so it needs a confirmation prompt and an idempotency
      key (see the spec).
