// OAuth blob format shared by the auth binary (issuer) and the proxy
// (verifier): "<prefix><base64url(payloadJSON)>.<base64url(mac)>" where
// mac = HMAC-SHA256(secret, tag + "\x00" + payloadJSON). The per-type tag
// gives domain separation — a blob signed as one type can never verify as
// another, and none of them collide with the pmgr_sso cookie format.
// Stateless like the cookie: revocation is rotating the shared secret.
package sso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// Blob prefixes. Doubles as the type discriminator on the wire.
const (
	ClientIDPrefix     = "pmgcl_"
	AuthCodePrefix     = "pmgc_"
	AccessTokenPrefix  = "pmga_"
	RefreshTokenPrefix = "pmgf_"
)

// Per-type HMAC tags (never on the wire, only mixed into the MAC).
const (
	tagClient  = "pmgr-oauth-client-v1"
	tagCode    = "pmgr-oauth-code-v1"
	tagAccess  = "pmgr-oauth-at-v1"
	tagRefresh = "pmgr-oauth-rt-v1"
)

func blobMAC(tag string, payload, secret []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(tag))
	mac.Write([]byte{0})
	mac.Write(payload)
	return mac.Sum(nil)
}

func signBlob(prefix, tag string, payload, secret []byte) string {
	return prefix + base64.RawURLEncoding.EncodeToString(payload) +
		"." + base64.RawURLEncoding.EncodeToString(blobMAC(tag, payload, secret))
}

func verifyBlob(raw, prefix, tag string, secret []byte) ([]byte, bool) {
	body, found := strings.CutPrefix(raw, prefix)
	if !found {
		return nil, false
	}
	payB64, macB64, found := strings.Cut(body, ".")
	if !found {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(payB64)
	if err != nil {
		return nil, false
	}
	sig, err := base64.RawURLEncoding.DecodeString(macB64)
	if err != nil {
		return nil, false
	}
	if !hmac.Equal(blobMAC(tag, payload, secret), sig) {
		return nil, false
	}
	return payload, true
}

func expired(exp int64) bool { return time.Now().Unix() > exp }

// ClientReg is the payload of a pmgcl_ client_id issued by dynamic client
// registration. The client_id IS the registration — no server-side store.
type ClientReg struct {
	RedirectURIs []string `json:"ru"`
	Name         string   `json:"name"`
	IssuedAt     int64    `json:"iat"`
}

func SignClient(c ClientReg, secret []byte) string {
	payload, _ := json.Marshal(c)
	return signBlob(ClientIDPrefix, tagClient, payload, secret)
}

func VerifyClient(raw string, secret []byte) (ClientReg, bool) {
	payload, ok := verifyBlob(raw, ClientIDPrefix, tagClient, secret)
	var c ClientReg
	if !ok || json.Unmarshal(payload, &c) != nil {
		return ClientReg{}, false
	}
	return c, true
}

// CodeClaims is the payload of a pmgc_ authorization code.
type CodeClaims struct {
	Username      string `json:"u"`
	ClientID      string `json:"cid"`
	Audience      string `json:"aud"`
	Scope         string `json:"scope"`
	RedirectURI   string `json:"ru"`
	CodeChallenge string `json:"cc"`
	JTI           string `json:"jti"`
	Exp           int64  `json:"exp"`
}

func SignCode(c CodeClaims, secret []byte) string {
	payload, _ := json.Marshal(c)
	return signBlob(AuthCodePrefix, tagCode, payload, secret)
}

func VerifyCode(raw string, secret []byte) (CodeClaims, bool) {
	payload, ok := verifyBlob(raw, AuthCodePrefix, tagCode, secret)
	var c CodeClaims
	if !ok || json.Unmarshal(payload, &c) != nil || expired(c.Exp) {
		return CodeClaims{}, false
	}
	return c, true
}

// AccessClaims is the payload of a pmga_ access token.
type AccessClaims struct {
	Username string `json:"u"`
	ClientID string `json:"cid"`
	Audience string `json:"aud"`
	Scope    string `json:"scope"`
	Exp      int64  `json:"exp"`
}

func SignAccess(c AccessClaims, secret []byte) string {
	payload, _ := json.Marshal(c)
	return signBlob(AccessTokenPrefix, tagAccess, payload, secret)
}

func VerifyAccess(raw string, secret []byte) (AccessClaims, bool) {
	payload, ok := verifyBlob(raw, AccessTokenPrefix, tagAccess, secret)
	var c AccessClaims
	if !ok || json.Unmarshal(payload, &c) != nil || expired(c.Exp) {
		return AccessClaims{}, false
	}
	return c, true
}

// RefreshClaims is the payload of a pmgf_ refresh token. JTI enables
// single-use rotation via the auth binary's used-JTI set.
type RefreshClaims struct {
	Username string `json:"u"`
	ClientID string `json:"cid"`
	Audience string `json:"aud"`
	Scope    string `json:"scope"`
	JTI      string `json:"jti"`
	Exp      int64  `json:"exp"`
}

func SignRefresh(c RefreshClaims, secret []byte) string {
	payload, _ := json.Marshal(c)
	return signBlob(RefreshTokenPrefix, tagRefresh, payload, secret)
}

func VerifyRefresh(raw string, secret []byte) (RefreshClaims, bool) {
	payload, ok := verifyBlob(raw, RefreshTokenPrefix, tagRefresh, secret)
	var c RefreshClaims
	if !ok || json.Unmarshal(payload, &c) != nil || expired(c.Exp) {
		return RefreshClaims{}, false
	}
	return c, true
}
