// Package sso is the shared HMAC-signed cookie format between the auth
// binary (issuer) and the proxy (verifier). Cookie value:
// username|expiryUnix|hex(HMAC-SHA256("username|expiryUnix", secret)).
// Stateless by design — no server-side session store; revocation is
// rotating the shared secret.
package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// CookieName is the domain-wide SSO cookie set by the auth binary and
// checked by the proxy on proxy.auth=true hosts.
const CookieName = "pmgr_sso"

// Sign builds a cookie value for username expiring at exp. Usernames
// containing '|' produce values that Verify will always reject.
func Sign(username string, exp time.Time, secret []byte) string {
	payload := username + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(payload))
	return payload + "|" + hex.EncodeToString(mac.Sum(nil))
}

// Verify checks the signature and expiry of a cookie value. A username
// containing '|' would split into more than three parts and is rejected,
// so the delimiter can never be smuggled into the signed payload.
func Verify(raw string, secret []byte) (username string, ok bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 3 || parts[0] == "" {
		return "", false
	}
	expUnix, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", false
	}
	sig, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(parts[0] + "|" + parts[1]))
	if !hmac.Equal(mac.Sum(nil), sig) {
		return "", false
	}
	if time.Now().Unix() > expUnix {
		return "", false
	}
	return parts[0], true
}
