// Package codexauth reads and writes the Codex CLI's own sign-in,
// $CODEX_HOME/auth.json, so an account can be handed to Codex.
package codexauth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
)

// HomeEnvVar overrides Codex's home directory, as it does for Codex.
const HomeEnvVar = "CODEX_HOME"

// Home is a Codex home directory (default ~/.codex).
type Home struct {
	Dir string
}

// DefaultHome returns $CODEX_HOME, or ~/.codex.
func DefaultHome(getenv func(string) string) (Home, error) {
	if dir := getenv(HomeEnvVar); dir != "" {
		return Home{Dir: dir}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Home{}, fmt.Errorf("finding home directory: %w", err)
	}
	return Home{Dir: filepath.Join(home, ".codex")}, nil
}

// AuthPath is where Codex keeps its sign-in.
func (h Home) AuthPath() string { return filepath.Join(h.Dir, "auth.json") }

// Auth is the sign-in stored in auth.json.
type Auth struct {
	Mode        string // auth_mode; empty in older files
	HasAPIKey   bool   // OPENAI_API_KEY is set
	Tokens      *auth.Tokens
	AccountID   string // tokens.account_id
	LastRefresh time.Time

	raw map[string]json.RawMessage
}

// IsChatGPT reports whether this is a ChatGPT sign-in, using Codex's rules:
// an explicit auth_mode wins, otherwise an API key means API-key mode.
func (a *Auth) IsChatGPT() bool {
	mode := a.Mode
	if mode == "" {
		mode = "chatgpt"
		if a.HasAPIKey {
			mode = "apikey"
		}
	}
	return mode == "chatgpt" && a.Tokens != nil && a.Tokens.RefreshToken != ""
}

type fileTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
}

// Read loads auth.json. It returns nil, nil if Codex isn't signed in.
func (h Home) Read() (*Auth, error) {
	data, err := os.ReadFile(h.AuthPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading Codex sign-in: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", h.AuthPath(), err)
	}
	a := &Auth{raw: raw}
	json.Unmarshal(raw["auth_mode"], &a.Mode)
	var apiKey *string
	json.Unmarshal(raw["OPENAI_API_KEY"], &apiKey)
	a.HasAPIKey = apiKey != nil && *apiKey != ""
	var lastRefresh string
	if json.Unmarshal(raw["last_refresh"], &lastRefresh) == nil {
		a.LastRefresh, _ = time.Parse(time.RFC3339Nano, lastRefresh)
	}
	if t, ok := raw["tokens"]; ok && string(t) != "null" {
		var ft fileTokens
		if err := json.Unmarshal(t, &ft); err != nil {
			return nil, fmt.Errorf("parsing %s: tokens: %w", h.AuthPath(), err)
		}
		a.Tokens = &auth.Tokens{IDToken: ft.IDToken, AccessToken: ft.AccessToken, RefreshToken: ft.RefreshToken}
		a.AccountID = ft.AccountID
	}
	return a, nil
}

// SignIn replaces auth.json with a ChatGPT sign-in, in the format `codex
// login` writes.
func (h Home) SignIn(tokens auth.Tokens, accountID string, lastRefresh time.Time) error {
	doc := map[string]any{
		"auth_mode":      "chatgpt",
		"OPENAI_API_KEY": nil,
		"tokens":         fileTokens{tokens.IDToken, tokens.AccessToken, tokens.RefreshToken, accountID},
		"last_refresh":   lastRefresh.UTC().Format(time.RFC3339Nano),
	}
	return h.write(doc)
}

// UpdateTokens swaps new tokens into prev, keeping every other field.
func (h Home) UpdateTokens(prev *Auth, tokens auth.Tokens, lastRefresh time.Time) error {
	doc := map[string]any{}
	for k, v := range prev.raw {
		doc[k] = v
	}
	doc["tokens"] = fileTokens{tokens.IDToken, tokens.AccessToken, tokens.RefreshToken, prev.AccountID}
	doc["last_refresh"] = lastRefresh.UTC().Format(time.RFC3339Nano)
	return h.write(doc)
}

// Backup copies auth.json to dst, readable only by the owner.
func (h Home) Backup(dst string) error {
	data, err := os.ReadFile(h.AuthPath())
	if err != nil {
		return fmt.Errorf("backing up Codex sign-in: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return fmt.Errorf("backing up Codex sign-in: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return fmt.Errorf("backing up Codex sign-in: %w", err)
	}
	return nil
}

// write atomically replaces auth.json, so Codex never reads a partial file.
func (h Home) write(doc map[string]any) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(h.Dir, 0o700); err != nil {
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	// CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(h.Dir, ".auth-*.json")
	if err != nil {
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	if err := os.Rename(tmp.Name(), h.AuthPath()); err != nil {
		return fmt.Errorf("writing Codex sign-in: %w", err)
	}
	return nil
}

// StoreMode returns Codex's cli_auth_credentials_store setting from
// config.toml: "file" (the default), "keyring", "auto" or "ephemeral". Only
// top-level keys are read; this is not a full TOML parser.
func (h Home) StoreMode() (string, error) {
	data, err := os.ReadFile(filepath.Join(h.Dir, "config.toml"))
	if errors.Is(err, os.ErrNotExist) {
		return "file", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading Codex config: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			break // top-level keys come before the first table
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "cli_auth_credentials_store" {
			continue
		}
		value = strings.TrimSpace(value)
		if i := strings.Index(value, "#"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		return strings.ToLower(strings.Trim(value, `"'`)), nil
	}
	return "file", nil
}
