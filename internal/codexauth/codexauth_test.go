package codexauth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/AnandChowdhary/codex-usage/internal/auth"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRead(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	if a, err := h.Read(); a != nil || err != nil {
		t.Fatalf("missing auth.json: %v, %v", a, err)
	}

	tests := []struct {
		name, doc string
		chatgpt   bool
	}{
		{"chatgpt", `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"tokens":{"id_token":"i","access_token":"a","refresh_token":"r","account_id":"ws"},"last_refresh":"2026-09-28T10:00:00.123Z"}`, true},
		{"legacy chatgpt", `{"OPENAI_API_KEY":null,"tokens":{"id_token":"i","access_token":"a","refresh_token":"r"}}`, true},
		{"api key", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`, false},
		{"legacy api key with tokens", `{"OPENAI_API_KEY":"sk-x","tokens":{"id_token":"i","access_token":"a","refresh_token":"r"}}`, false},
		{"chatgpt mode with key", `{"auth_mode":"chatgpt","OPENAI_API_KEY":"sk-x","tokens":{"id_token":"i","access_token":"a","refresh_token":"r"}}`, true},
		{"no tokens", `{"auth_mode":"chatgpt","tokens":null}`, false},
	}
	for _, tt := range tests {
		writeFile(t, h.AuthPath(), tt.doc)
		a, err := h.Read()
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if a.IsChatGPT() != tt.chatgpt {
			t.Errorf("%s: IsChatGPT = %v", tt.name, a.IsChatGPT())
		}
	}

	writeFile(t, h.AuthPath(), `{"auth_mode":"chatgpt","tokens":{"id_token":"i","access_token":"a","refresh_token":"r","account_id":"ws"},"last_refresh":"2026-09-28T10:00:00.123Z"}`)
	a, _ := h.Read()
	if *a.Tokens != (auth.Tokens{IDToken: "i", AccessToken: "a", RefreshToken: "r"}) || a.AccountID != "ws" ||
		!a.LastRefresh.Equal(time.Date(2026, 9, 28, 10, 0, 0, 123e6, time.UTC)) {
		t.Fatalf("parsed %+v", a)
	}

	writeFile(t, h.AuthPath(), `{"tokens": `)
	if _, err := h.Read(); err == nil {
		t.Fatal("parsed a truncated auth.json")
	}
}

func TestSignInAndUpdateTokens(t *testing.T) {
	h := Home{Dir: filepath.Join(t.TempDir(), "codex")}
	at := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	if err := h.SignIn(auth.Tokens{IDToken: "i", AccessToken: "a", RefreshToken: "r"}, "ws", at); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	data, _ := os.ReadFile(h.AuthPath())
	json.Unmarshal(data, &doc)
	tokens := doc["tokens"].(map[string]any)
	if doc["auth_mode"] != "chatgpt" || doc["OPENAI_API_KEY"] != nil || tokens["refresh_token"] != "r" ||
		tokens["account_id"] != "ws" || doc["last_refresh"] != "2026-09-28T12:00:00Z" {
		t.Fatalf("auth.json = %s", data)
	}
	if runtime.GOOS != "windows" {
		if info, _ := os.Stat(h.AuthPath()); info.Mode().Perm() != 0o600 {
			t.Fatalf("auth.json mode %o", info.Mode().Perm())
		}
	}

	// Updating tokens keeps fields codex-usage doesn't know about.
	writeFile(t, h.AuthPath(), `{"auth_mode":"chatgpt","OPENAI_API_KEY":null,"future_field":{"x":1},"tokens":{"id_token":"i","access_token":"a","refresh_token":"r","account_id":"ws"}}`)
	prev, _ := h.Read()
	if err := h.UpdateTokens(prev, auth.Tokens{IDToken: "i2", AccessToken: "a2", RefreshToken: "r2"}, at); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(h.AuthPath())
	doc = nil
	json.Unmarshal(data, &doc)
	tokens = doc["tokens"].(map[string]any)
	if doc["future_field"] == nil || tokens["refresh_token"] != "r2" || tokens["account_id"] != "ws" {
		t.Fatalf("after update: %s", data)
	}
	entries, _ := os.ReadDir(h.Dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestStoreMode(t *testing.T) {
	h := Home{Dir: t.TempDir()}
	tests := map[string]string{
		"":                                       "file",
		`model = "gpt-6"`:                        "file",
		`cli_auth_credentials_store = "keyring"`: "keyring",
		"cli_auth_credentials_store='auto' # use the keychain":                     "auto",
		"model = \"x\"\n[profiles.work]\ncli_auth_credentials_store = \"keyring\"": "file",
	}
	for config, want := range tests {
		writeFile(t, filepath.Join(h.Dir, "config.toml"), config)
		if got, err := h.StoreMode(); err != nil || got != want {
			t.Errorf("config %q: StoreMode = %q, %v; want %q", config, got, err, want)
		}
	}
	os.Remove(filepath.Join(h.Dir, "config.toml"))
	if got, _ := h.StoreMode(); got != "file" {
		t.Errorf("no config: %q", got)
	}
}
