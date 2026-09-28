package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func makeJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
}

func TestParseIdentity(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"email": "me@example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_plan_type":          "pro",
			"chatgpt_user_id":            "user-1",
			"chatgpt_account_id":         "acct-1",
			"chatgpt_account_is_fedramp": true,
		},
	})
	got, err := ParseIdentity(token)
	if err != nil {
		t.Fatal(err)
	}
	want := Identity{Email: "me@example.com", PlanType: "pro", UserID: "user-1", AccountID: "acct-1", FedRAMP: true}
	if got != want {
		t.Fatalf("ParseIdentity = %+v, want %+v", got, want)
	}
}

func TestParseIdentityFallbacks(t *testing.T) {
	token := makeJWT(t, map[string]any{
		"https://api.openai.com/profile": map[string]any{"email": "profile@example.com"},
		"https://api.openai.com/auth":    map[string]any{"user_id": "legacy-user"},
	})
	got, err := ParseIdentity(token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Email != "profile@example.com" || got.UserID != "legacy-user" {
		t.Fatalf("ParseIdentity = %+v", got)
	}
	if _, err := ParseIdentity("not-a-jwt"); err == nil {
		t.Fatal("ParseIdentity accepted a malformed token")
	}
}

func TestTokenExpiry(t *testing.T) {
	exp, ok := TokenExpiry(makeJWT(t, map[string]any{"exp": 1790000000}))
	if !ok || !exp.Equal(time.Unix(1790000000, 0)) {
		t.Fatalf("TokenExpiry = %v, %v", exp, ok)
	}
	if _, ok := TokenExpiry(makeJWT(t, map[string]any{})); ok {
		t.Fatal("TokenExpiry found an exp claim that isn't there")
	}
	if _, ok := TokenExpiry("opaque-token"); ok {
		t.Fatal("TokenExpiry parsed an opaque token")
	}
}

func TestDeviceLogin(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["client_id"] != ClientID {
				t.Errorf("usercode client_id = %q", body["client_id"])
			}
			io.WriteString(w, `{"device_auth_id":"dev-1","usercode":"ABCD-1234","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["device_auth_id"] != "dev-1" || body["user_code"] != "ABCD-1234" {
				t.Errorf("poll body = %v", body)
			}
			if polls.Add(1) < 3 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			io.WriteString(w, `{"authorization_code":"auth-code","code_challenge":"challenge","code_verifier":"verifier"}`)
		case "/oauth/token":
			if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
				t.Errorf("exchange content type = %q", ct)
			}
			r.ParseForm()
			want := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {ClientID},
				"code":          {"auth-code"},
				"redirect_uri":  {"http://" + r.Host + "/deviceauth/callback"},
				"code_verifier": {"verifier"},
			}
			for k, v := range want {
				if r.PostForm.Get(k) != v[0] {
					t.Errorf("exchange %s = %q, want %q", k, r.PostForm.Get(k), v[0])
				}
			}
			io.WriteString(w, `{"id_token":"id","access_token":"access","refresh_token":"refresh"}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusTeapot)
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL, srv.Client(), "test")
	dc, err := c.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.UserCode != "ABCD-1234" || dc.VerificationURL != srv.URL+"/codex/device" || dc.interval != 5*time.Second {
		t.Fatalf("device code = %+v", dc)
	}
	dc.interval = time.Millisecond
	tokens, err := c.CompleteDeviceLogin(context.Background(), dc)
	if err != nil {
		t.Fatal(err)
	}
	if *tokens != (Tokens{IDToken: "id", AccessToken: "access", RefreshToken: "refresh"}) {
		t.Fatalf("tokens = %+v", tokens)
	}
	if polls.Load() != 3 {
		t.Fatalf("polled %d times, want 3", polls.Load())
	}
}

func TestRequestDeviceCodeDisabled(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	_, err := NewClient(srv.URL, srv.Client(), "").RequestDeviceCode(context.Background())
	if !errors.Is(err, ErrDeviceAuthDisabled) {
		t.Fatalf("err = %v, want ErrDeviceAuthDisabled", err)
	}
}

func TestDeviceCodeIntervalAsNumber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"device_auth_id":"d","user_code":"C","interval":7}`)
	}))
	defer srv.Close()
	dc, err := NewClient(srv.URL, srv.Client(), "").RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if dc.interval != 7*time.Second {
		t.Fatalf("interval = %v", dc.interval)
	}
}

func TestDeviceCodeExpires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	dc := &DeviceCode{UserCode: "C", deviceAuthID: "d", interval: time.Millisecond, ExpiresAt: time.Now().Add(20 * time.Millisecond)}
	_, err := NewClient(srv.URL, srv.Client(), "").CompleteDeviceLogin(context.Background(), dc)
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("err = %v, want ErrDeviceCodeExpired", err)
	}
}

func TestDeviceCodeRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"code":"denied","message":"user denied access"}}`)
	}))
	defer srv.Close()
	dc := &DeviceCode{UserCode: "C", deviceAuthID: "d", interval: time.Millisecond}
	_, err := NewClient(srv.URL, srv.Client(), "").CompleteDeviceLogin(context.Background(), dc)
	if err == nil || err.Error() != "waiting for sign-in approval failed: HTTP 400: user denied access" {
		t.Fatalf("err = %v", err)
	}
}

func TestRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("refresh content type = %q", ct)
		}
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		want := map[string]string{"grant_type": "refresh_token", "client_id": ClientID, "refresh_token": "old"}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("refresh %s = %q, want %q", k, body[k], v)
			}
		}
		io.WriteString(w, `{"access_token":"new-access","refresh_token":"new-refresh"}`)
	}))
	defer srv.Close()
	tokens, err := NewClient(srv.URL, srv.Client(), "").Refresh(context.Background(), "old")
	if err != nil {
		t.Fatal(err)
	}
	if *tokens != (Tokens{AccessToken: "new-access", RefreshToken: "new-refresh"}) {
		t.Fatalf("tokens = %+v", tokens)
	}
}

func TestRefreshErrors(t *testing.T) {
	tests := []struct {
		status    int
		body      string
		permanent bool
	}{
		{http.StatusUnauthorized, `{"error":"whatever"}`, true},
		{http.StatusBadRequest, `{"error":"invalid_grant"}`, true},
		{http.StatusBadRequest, `{"error":{"code":"refresh_token_reused","message":"used"}}`, true},
		{http.StatusForbidden, `{"code":"refresh_token_expired"}`, true},
		{http.StatusBadRequest, `{"error":"refresh_token_invalidated"}`, true},
		{http.StatusInternalServerError, `oops`, false},
		{http.StatusBadRequest, `{"error":"invalid_request"}`, false},
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.status)
			io.WriteString(w, tt.body)
		}))
		_, err := NewClient(srv.URL, srv.Client(), "").Refresh(context.Background(), "old")
		srv.Close()
		var re *RefreshError
		if !errors.As(err, &re) {
			t.Fatalf("%d %s: err = %v, want *RefreshError", tt.status, tt.body, err)
		}
		if re.Permanent != tt.permanent {
			t.Errorf("%d %s: permanent = %v, want %v", tt.status, tt.body, re.Permanent, tt.permanent)
		}
	}
}

func TestRevoke(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/revoke" {
			t.Errorf("path = %s", r.URL.Path)
		}
		json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := NewClient(srv.URL, srv.Client(), "").Revoke(context.Background(), "rt"); err != nil {
		t.Fatal(err)
	}
	if got["token"] != "rt" || got["token_type_hint"] != "refresh_token" || got["client_id"] != ClientID {
		t.Fatalf("revoke body = %v", got)
	}
}
