package store

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
)

func account(user, workspace, email string) Account {
	return Account{UserID: user, AccountID: workspace, Email: email, Tokens: auth.Tokens{AccessToken: "a-" + workspace}}
}

func TestSaveLoad(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "nested", "accounts.json")}
	f, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 0 {
		t.Fatalf("missing file loaded %d accounts", len(f.Accounts))
	}

	added := time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC)
	a := account("u1", "w1", "me@example.com")
	a.AddedAt = added
	f.Upsert(a, "")
	if err := s.Save(f); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Accounts) != 1 || got.Accounts[0].Label != "me@example.com" || !got.Accounts[0].AddedAt.Equal(added) {
		t.Fatalf("loaded %+v", got.Accounts)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(s.Path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("accounts file mode = %o, want 600", perm)
		}
		dir, _ := os.Stat(filepath.Dir(s.Path))
		if perm := dir.Mode().Perm(); perm != 0o700 {
			t.Fatalf("accounts dir mode = %o, want 700", perm)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(s.Path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestLoadRejectsNewerVersion(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "accounts.json")}
	os.WriteFile(s.Path, []byte(`{"version": 99, "accounts": []}`), 0o600)
	if _, err := s.Load(); err == nil {
		t.Fatal("loaded a file from a newer version")
	}
}

func TestUpsert(t *testing.T) {
	f := &File{}
	first := account("u1", "personal", "me@example.com")
	first.AddedAt = time.Unix(100, 0)
	if got := f.Upsert(first, ""); got.Label != "me@example.com" {
		t.Fatalf("default label = %q", got.Label)
	}

	// Same email, other workspace: a separate account with a unique label.
	if got := f.Upsert(account("u1", "team", "me@example.com"), ""); got.Label != "me@example.com-2" {
		t.Fatalf("second workspace label = %q", got.Label)
	}

	// Signing in again replaces tokens but keeps the label and AddedAt.
	again := account("u1", "personal", "me@example.com")
	again.Tokens.AccessToken = "fresh"
	got := f.Upsert(again, "")
	if len(f.Accounts) != 2 || got.Label != "me@example.com" || got.Tokens.AccessToken != "fresh" || !got.AddedAt.Equal(time.Unix(100, 0)) {
		t.Fatalf("re-login stored %+v (accounts %d)", got, len(f.Accounts))
	}

	// A requested label renames the account, unless another account owns it.
	if got := f.Upsert(account("u1", "personal", "me@example.com"), "home"); got.Label != "home" {
		t.Fatalf("requested label = %q", got.Label)
	}
	if got := f.Upsert(account("u2", "other", "you@example.com"), "home"); got.Label != "home-2" {
		t.Fatalf("colliding requested label = %q", got.Label)
	}
}

func TestFindAndRemove(t *testing.T) {
	f := &File{}
	f.Upsert(account("u1", "w1", "me@example.com"), "work")
	f.Upsert(account("u1", "w2", "me@example.com"), "")
	f.Upsert(account("u2", "w3", "other@example.com"), "me@example.com-x")

	if got := f.Find("work"); len(got) != 1 || got[0] != 0 {
		t.Fatalf("Find(label) = %v", got)
	}
	if got := f.Find("ME@example.com"); len(got) != 2 {
		t.Fatalf("Find(email) = %v, want both workspaces", got)
	}
	if got := f.Find("nobody"); len(got) != 0 {
		t.Fatalf("Find(unknown) = %v", got)
	}
	f.Remove(0)
	if len(f.Accounts) != 2 || f.Accounts[0].AccountID != "w2" {
		t.Fatalf("after Remove: %+v", f.Accounts)
	}
}

func TestLockIsExclusive(t *testing.T) {
	s := &Store{Path: filepath.Join(t.TempDir(), "accounts.json")}
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := s.Lock(ctx); err == nil {
		t.Fatal("second Lock succeeded while the first was held")
	}

	unlock()
	unlock2, err := s.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock after unlock: %v", err)
	}
	unlock2()
}
