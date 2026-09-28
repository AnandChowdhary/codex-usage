package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Identity is the account information carried in a ChatGPT ID token.
type Identity struct {
	Email     string
	PlanType  string
	UserID    string
	AccountID string // ChatGPT workspace
	FedRAMP   bool
}

type idClaims struct {
	Email   string `json:"email"`
	Profile *struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
	Auth *struct {
		PlanType  string `json:"chatgpt_plan_type"`
		UserID    string `json:"chatgpt_user_id"`
		LegacyID  string `json:"user_id"`
		AccountID string `json:"chatgpt_account_id"`
		FedRAMP   bool   `json:"chatgpt_account_is_fedramp"`
	} `json:"https://api.openai.com/auth"`
}

// ParseIdentity reads the identity claims from an ID token. The signature is
// not verified: the claims are only used for display and bookkeeping.
func ParseIdentity(idToken string) (Identity, error) {
	var claims idClaims
	if err := decodeJWTPayload(idToken, &claims); err != nil {
		return Identity{}, err
	}
	id := Identity{Email: claims.Email}
	if id.Email == "" && claims.Profile != nil {
		id.Email = claims.Profile.Email
	}
	if a := claims.Auth; a != nil {
		id.PlanType = a.PlanType
		id.UserID = a.UserID
		if id.UserID == "" {
			id.UserID = a.LegacyID
		}
		id.AccountID = a.AccountID
		id.FedRAMP = a.FedRAMP
	}
	return id, nil
}

// TokenExpiry returns the exp claim of a JWT, if it has one.
func TokenExpiry(token string) (time.Time, bool) {
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if decodeJWTPayload(token, &claims) != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

func decodeJWTPayload(token string, v any) error {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return errors.New("invalid JWT format")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}
