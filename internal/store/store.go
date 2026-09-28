// Package store persists signed-in accounts to a JSON file.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
)

const (
	// FileVersion is the accounts file format written by this build.
	FileVersion = 1
	// HomeEnvVar overrides the directory holding the accounts file.
	HomeEnvVar = "CODEX_USAGE_HOME"

	fileName = "accounts.json"
)

// Account is one signed-in ChatGPT account and workspace.
type Account struct {
	Label        string      `json:"label"`
	Email        string      `json:"email,omitempty"`
	PlanType     string      `json:"plan_type,omitempty"`
	UserID       string      `json:"chatgpt_user_id,omitempty"`
	AccountID    string      `json:"chatgpt_account_id,omitempty"`
	FedRAMP      bool        `json:"fedramp,omitempty"`
	Tokens       auth.Tokens `json:"tokens"`
	LastRefresh  time.Time   `json:"last_refresh"`
	AddedAt      time.Time   `json:"added_at"`
	NeedsRelogin bool        `json:"needs_relogin,omitempty"`
	// InCodex marks the account whose session was handed to the Codex CLI
	// with `switch`. Codex's auth.json then holds the same session, and the
	// newer copy wins whenever they differ.
	InCodex bool `json:"in_codex,omitempty"`
}

// Key identifies the account across logins. One user can sign in to several
// workspaces, so the workspace is part of the key.
func (a Account) Key() string {
	if a.UserID == "" && a.AccountID == "" {
		return "email:" + strings.ToLower(a.Email)
	}
	return a.UserID + "/" + a.AccountID
}

// ApplyIdentity copies identity claims onto the account.
func (a *Account) ApplyIdentity(id auth.Identity) {
	if id.Email != "" {
		a.Email = id.Email
	}
	if id.PlanType != "" {
		a.PlanType = id.PlanType
	}
	if a.UserID == "" {
		a.UserID = id.UserID
	}
	if a.AccountID == "" {
		a.AccountID = id.AccountID
	}
	a.FedRAMP = id.FedRAMP
}

// File is the on-disk accounts file.
type File struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Index returns the position of the account with key, or -1.
func (f *File) Index(key string) int {
	for i, a := range f.Accounts {
		if a.Key() == key {
			return i
		}
	}
	return -1
}

// Upsert stores a, replacing any account with the same identity. The existing
// label is kept unless label is non-empty; labels are made unique. It returns
// the stored account.
func (f *File) Upsert(a Account, label string) Account {
	key := a.Key()
	idx := f.Index(key)
	if idx >= 0 {
		a.AddedAt = f.Accounts[idx].AddedAt
		if label == "" {
			label = f.Accounts[idx].Label
		}
	}
	if label == "" {
		label = a.Email
	}
	if label == "" {
		label = "account"
	}
	a.Label = f.uniqueLabel(label, key)
	if idx >= 0 {
		f.Accounts[idx] = a
	} else {
		f.Accounts = append(f.Accounts, a)
	}
	return a
}

func (f *File) uniqueLabel(label, key string) string {
	taken := func(l string) bool {
		for _, a := range f.Accounts {
			if a.Label == l && a.Key() != key {
				return true
			}
		}
		return false
	}
	candidate := label
	for n := 2; taken(candidate); n++ {
		candidate = label + "-" + strconv.Itoa(n)
	}
	return candidate
}

// Find resolves a label or email to account positions. An exact label match
// wins; otherwise every account with that email matches.
func (f *File) Find(query string) []int {
	for i, a := range f.Accounts {
		if a.Label == query {
			return []int{i}
		}
	}
	var matches []int
	for i, a := range f.Accounts {
		if a.Email != "" && strings.EqualFold(a.Email, query) {
			matches = append(matches, i)
		}
	}
	return matches
}

// Remove deletes the account at position i.
func (f *File) Remove(i int) {
	f.Accounts = append(f.Accounts[:i], f.Accounts[i+1:]...)
}

// Store reads and writes the accounts file at Path.
type Store struct {
	Path string
}

// DefaultPath returns $CODEX_USAGE_HOME/accounts.json, or ~/.codex-usage/accounts.json.
func DefaultPath(getenv func(string) string) (string, error) {
	if dir := getenv(HomeEnvVar); dir != "" {
		return filepath.Join(dir, fileName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("finding home directory: %w", err)
	}
	return filepath.Join(home, ".codex-usage", fileName), nil
}

// Load reads the accounts file. A missing file is an empty account list.
func (s *Store) Load() (*File, error) {
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return &File{Version: FileVersion}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading accounts: %w", err)
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", s.Path, err)
	}
	if f.Version > FileVersion {
		return nil, fmt.Errorf("%s was written by a newer codex-usage (format %d); upgrade to use it", s.Path, f.Version)
	}
	f.Version = FileVersion
	return &f, nil
}

// Save atomically replaces the accounts file. The file holds credentials, so
// it is only readable by the owner.
func (s *Store) Save(f *File) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	f.Version = FileVersion
	if f.Accounts == nil {
		f.Accounts = []Account{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// CreateTemp creates the file with mode 0600.
	tmp, err := os.CreateTemp(dir, ".accounts-*.json")
	if err != nil {
		return fmt.Errorf("saving accounts: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("saving accounts: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("saving accounts: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("saving accounts: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return fmt.Errorf("saving accounts: %w", err)
	}
	return nil
}

// Lock takes an exclusive lock shared by every codex-usage process using this
// file. Hold it across load-modify-save so two processes never spend the same
// refresh token.
func (s *Store) Lock(ctx context.Context) (unlock func(), err error) {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	return lockFile(ctx, s.Path+".lock")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
