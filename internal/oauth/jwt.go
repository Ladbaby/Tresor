package oauth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// jwtExp extracts the "exp" claim (unix seconds) from a JWT without
// verification. Returns 0 when the token is not a JWT or has no exp claim.
func jwtExp(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		Exp *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == nil {
		return 0
	}
	return *claims.Exp
}

// tokenExpiry computes the absolute expiry of a token. When expiresAt (from
// expires_in) is set it wins; otherwise the JWT exp claim is the fallback.
// Returns zero when the expiry is unknown.
func tokenExpiry(accessToken string, expiresAt int64) time.Time {
	if expiresAt > 0 {
		return time.Unix(expiresAt, 0)
	}
	if exp := jwtExp(accessToken); exp > 0 {
		return time.Unix(exp, 0)
	}
	return time.Time{}
}
