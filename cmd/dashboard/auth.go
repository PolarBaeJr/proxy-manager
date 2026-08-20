// Authentication: per-user password + TOTP 2FA.
//
// Model:
//   - First-run setup creates the initial user.
//   - Reads (GET) require a logged-in session (any user).
//   - Writes (POST/PATCH/DELETE) require a session elevated by a valid TOTP code
//     within the last 5 minutes.
//   - Existing users with elevation can add or remove other users.
//
// Stdlib only — PBKDF2-HMAC-SHA256 for passwords, RFC 6238 TOTP for 2FA,
// HMAC-SHA256 signed cookies for sessions.

package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookie     = "goproxy_session"
	sessionLifetime   = 8 * time.Hour
	elevatedLifetime  = sessionLifetime // once you 2FA, edits stay unlocked for the rest of the session
	pbkdf2Iterations  = 200_000
	pbkdf2KeyLen      = 32
	totpDigits        = 6
	totpPeriodSeconds = 30
	totpAllowedDrift  = 1
)

type User struct {
	Username     string             `json:"username"`
	Salt         string             `json:"salt"`
	PasswordHash string             `json:"password_hash"`
	TOTPSecret   string             `json:"totp_secret"`
	CreatedAt    int64              `json:"created_at"`
	Tokens       []APIToken         `json:"tokens,omitempty"`
	Credentials  []StoredCredential `json:"credentials,omitempty"`
}

// APIToken is a programmatic credential. The raw token is shown ONCE at
// creation time and never stored — we only keep its SHA-256 hash.
// Token format: pmt_<base64url-of-32-random-bytes>
type APIToken struct {
	ID         string `json:"id"`    // short hex prefix for display + delete lookup
	Label      string `json:"label"` // user-supplied description
	Hash       string `json:"hash"`  // hex-encoded sha256(rawToken)
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

const apiTokenPrefix = "pmt_"

// internalUser is the principal for process-local calls. It only ever appears
// in an audit record as the "via" half; the actor assertion supplies the human.
const internalUser = "mcp"

type AuthStore struct {
	path    string
	mu      sync.RWMutex
	data    authData
	pending map[string]*pendingUser // keyed by lowercased username
}

// pendingUser holds a not-yet-confirmed user. They must verify a TOTP code
// matching their freshly-generated secret before they're saved to disk.
type pendingUser struct {
	user      User
	expiresAt time.Time
}

const pendingTTL = 10 * time.Minute

type authData struct {
	CookieSecret  string     `json:"cookie_secret"`
	Users         []User     `json:"users"`
	ServiceTokens []APIToken `json:"service_tokens,omitempty"`

	// Legacy single-user fields (migrated to Users on first load if found).
	LegacySalt         string `json:"salt,omitempty"`
	LegacyPasswordHash string `json:"password_hash,omitempty"`
	LegacyTOTPSecret   string `json:"totp_secret,omitempty"`
	LegacyCreatedAt    int64  `json:"created_at,omitempty"`
}

func loadAuthStore(path string) (*AuthStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &AuthStore{path: path, pending: map[string]*pendingUser{}}
	b, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(b, &s.data); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		s.migrateIfNeeded()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

// migrateIfNeeded converts the old single-user schema into a Users[]= entry.
func (s *AuthStore) migrateIfNeeded() {
	if len(s.data.Users) == 0 && s.data.LegacyPasswordHash != "" {
		s.data.Users = []User{{
			Username:     "admin",
			Salt:         s.data.LegacySalt,
			PasswordHash: s.data.LegacyPasswordHash,
			TOTPSecret:   s.data.LegacyTOTPSecret,
			CreatedAt:    s.data.LegacyCreatedAt,
		}}
		s.data.LegacySalt = ""
		s.data.LegacyPasswordHash = ""
		s.data.LegacyTOTPSecret = ""
		s.data.LegacyCreatedAt = 0
		_ = s.save()
	}
}

func (s *AuthStore) IsSetup() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.data.Users) > 0
}

func (s *AuthStore) save() error {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func (s *AuthStore) findUser(username string) *User {
	for i := range s.data.Users {
		if strings.EqualFold(s.data.Users[i].Username, username) {
			return &s.data.Users[i]
		}
	}
	return nil
}

// ---- Two-phase user creation: generate → confirm with TOTP ----

// BeginSetup queues the first user. The user is NOT saved until ConfirmPending
// succeeds with a TOTP code matching the returned secret.
func (s *AuthStore) BeginSetup(username, password string) (secret, otpauthURI string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Users) > 0 {
		return "", "", fmt.Errorf("already set up")
	}
	// Initialize the cookie secret now so login can issue cookies once the user confirms.
	if s.data.CookieSecret == "" {
		cookieB := make([]byte, 32)
		rand.Read(cookieB)
		s.data.CookieSecret = hex.EncodeToString(cookieB)
		if err := s.save(); err != nil {
			return "", "", err
		}
	}
	u, secret, uri, err := newUserInternal(username, password)
	if err != nil {
		return "", "", err
	}
	s.pending[strings.ToLower(username)] = &pendingUser{user: u, expiresAt: time.Now().Add(pendingTTL)}
	return secret, uri, nil
}

// BeginCreateUser queues an additional user pending TOTP confirmation.
func (s *AuthStore) BeginCreateUser(username, password string) (secret, otpauthURI string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.findUser(username) != nil {
		return "", "", fmt.Errorf("user %q already exists", username)
	}
	u, secret, uri, err := newUserInternal(username, password)
	if err != nil {
		return "", "", err
	}
	s.pending[strings.ToLower(username)] = &pendingUser{user: u, expiresAt: time.Now().Add(pendingTTL)}
	return secret, uri, nil
}

// ConfirmPending checks the TOTP code against the pending user's secret and,
// on success, persists the user.
func (s *AuthStore) ConfirmPending(username, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(username)
	p, ok := s.pending[key]
	if !ok {
		return fmt.Errorf("no pending confirmation for %q (already confirmed, or expired)", username)
	}
	if time.Now().After(p.expiresAt) {
		delete(s.pending, key)
		return fmt.Errorf("confirmation expired — start over")
	}
	if !verifyTOTPSecret(p.user.TOTPSecret, code) {
		return fmt.Errorf("invalid code")
	}
	s.data.Users = append(s.data.Users, p.user)
	delete(s.pending, key)
	return s.save()
}

// ---- API tokens (programmatic credentials) ----

// CreateToken generates a new API token for `username`. Returns the RAW token
// (only time it's ever shown) and the stored APIToken metadata.
func (s *AuthStore) CreateToken(username, label string) (rawToken string, t APIToken, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		return "", APIToken{}, fmt.Errorf("user %q not found", username)
	}
	if label == "" {
		label = "untitled"
	}
	rb := make([]byte, 32)
	rand.Read(rb)
	raw := apiTokenPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rb)
	h := sha256.Sum256([]byte(raw))
	hashHex := hex.EncodeToString(h[:])
	t = APIToken{
		ID:        hashHex[:12],
		Label:     label,
		Hash:      hashHex,
		CreatedAt: time.Now().Unix(),
	}
	u.Tokens = append(u.Tokens, t)
	if err := s.save(); err != nil {
		return "", APIToken{}, err
	}
	return raw, t, nil
}

// RemintServiceToken mints a fresh, persisted credential for a
// process-identity `name` (e.g. "statusbot") — NOT tied to a human User
// like CreateToken is. Any existing entry for `name` is replaced first: a
// token's raw value can never be recovered once minted (only its hash is
// stored), so if the caller's token file went missing there is no way to
// rewrite the same secret — minting fresh and orphaning the old hash is the
// only option.
func (s *AuthStore) RemintServiceToken(name string) (rawToken string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.data.ServiceTokens[:0]
	for _, t := range s.data.ServiceTokens {
		if t.Label != name {
			kept = append(kept, t)
		}
	}
	s.data.ServiceTokens = kept

	rb := make([]byte, 32)
	if _, err := rand.Read(rb); err != nil {
		return "", err
	}
	raw := apiTokenPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(rb)
	h := sha256.Sum256([]byte(raw))
	hashHex := hex.EncodeToString(h[:])
	s.data.ServiceTokens = append(s.data.ServiceTokens, APIToken{
		ID:        hashHex[:12],
		Label:     name,
		Hash:      hashHex,
		CreatedAt: time.Now().Unix(),
	})
	if err := s.save(); err != nil {
		return "", err
	}
	return raw, nil
}

// internalToken is a credential the process mints for itself at startup so
// in-process callers (the MCP handler) can go through the real API handlers
// instead of reimplementing them — keeping guardUnscalable, auto-onboard,
// canary bookkeeping and the audit log in one place.
//
// Held in memory only: never written to disk, never logged, never shown in the
// UI, and regenerated on every restart. There is nothing for an operator to
// manage and nothing on disk to steal. The person behind the call is carried
// separately by the actor assertion, so the audit log still names them.
var internalToken string

// mintInternalToken generates the process-local credential. Called once at
// startup; a generation failure leaves it empty, which disables the in-process
// path rather than falling back to something weaker.
func mintInternalToken() error {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	internalToken = apiTokenPrefix + "internal_" + hex.EncodeToString(b)
	return nil
}

// VerifyToken hashes the raw token and looks for a matching credential
// across the internal token, service tokens, and every user's tokens.
// Returns the owning identity if found, "" otherwise. This is the BROAD
// check (requireAuth): it accepts service tokens, which are deliberately
// non-elevating — see VerifyElevatedToken for the narrower one requireAuth's
// write-gated sibling uses.
func (s *AuthStore) VerifyToken(raw string) string {
	id, _ := s.verifyTokenKind(raw)
	return id
}

// VerifyElevatedToken is VerifyToken restricted to credentials authorized for
// elevated (write) actions. Auto-provisioned service tokens (see
// RemintServiceToken) verify fine under VerifyToken — a sibling container
// like statusbot needs to read status data — but must NOT pass here: nothing
// should auto-distribute a credential that can stage/replace/scale services
// or touch DNS just because a container mount exists.
func (s *AuthStore) VerifyElevatedToken(raw string) string {
	id, elevated := s.verifyTokenKind(raw)
	if !elevated {
		return ""
	}
	return id
}

func (s *AuthStore) verifyTokenKind(raw string) (id string, elevated bool) {
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		return "", false
	}
	// Constant-time so a caller cannot probe for the internal credential by
	// timing, same as the stored-token comparisons below.
	if internalToken != "" && subtle.ConstantTimeCompare([]byte(raw), []byte(internalToken)) == 1 {
		return internalUser, true
	}
	h := sha256.Sum256([]byte(raw))
	hashHex := hex.EncodeToString(h[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.data.ServiceTokens {
		if subtle.ConstantTimeCompare([]byte(s.data.ServiceTokens[i].Hash), []byte(hashHex)) == 1 {
			s.data.ServiceTokens[i].LastUsedAt = time.Now().Unix()
			_ = s.save()
			return s.data.ServiceTokens[i].Label, false
		}
	}
	for i := range s.data.Users {
		for j := range s.data.Users[i].Tokens {
			if subtle.ConstantTimeCompare([]byte(s.data.Users[i].Tokens[j].Hash), []byte(hashHex)) == 1 {
				s.data.Users[i].Tokens[j].LastUsedAt = time.Now().Unix()
				_ = s.save()
				return s.data.Users[i].Username, true
			}
		}
	}
	return "", false
}

func (s *AuthStore) ListTokens(username string) []APIToken {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.findUser(username)
	if u == nil {
		return nil
	}
	out := make([]APIToken, 0, len(u.Tokens))
	for _, t := range u.Tokens {
		t.Hash = "" // never leak the hash through the API
		out = append(out, t)
	}
	return out
}

func (s *AuthStore) DeleteToken(username, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := s.findUser(username)
	if u == nil {
		return fmt.Errorf("user %q not found", username)
	}
	for i, t := range u.Tokens {
		if t.ID == id {
			u.Tokens = append(u.Tokens[:i], u.Tokens[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("token %q not found", id)
}

// CancelPending throws away any pending user with the given username. Safe to call.
func (s *AuthStore) CancelPending(username string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, strings.ToLower(username))
}

// HasPending reports whether a username has an active pending confirmation.
func (s *AuthStore) HasPending(username string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.pending[strings.ToLower(username)]
	return ok && time.Now().Before(p.expiresAt)
}

func (s *AuthStore) DeleteUser(username string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Users) <= 1 {
		return fmt.Errorf("cannot delete the last user")
	}
	for i, u := range s.data.Users {
		if strings.EqualFold(u.Username, username) {
			s.data.Users = append(s.data.Users[:i], s.data.Users[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("user %q not found", username)
}

func (s *AuthStore) ListUsers() []User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]User, 0, len(s.data.Users))
	for _, u := range s.data.Users {
		// Don't leak hashes/secrets through the list endpoint.
		out = append(out, User{Username: u.Username, CreatedAt: u.CreatedAt})
	}
	return out
}

func newUserInternal(username, password string) (User, string, string, error) {
	if !validUsername(username) {
		return User{}, "", "", fmt.Errorf("username must be 2-32 chars, [a-zA-Z0-9._-]")
	}
	if len(password) < 8 {
		return User{}, "", "", fmt.Errorf("password must be at least 8 characters")
	}
	saltB := make([]byte, 16)
	rand.Read(saltB)
	secretB := make([]byte, 20)
	rand.Read(secretB)
	hash := pbkdf2([]byte(password), saltB, pbkdf2Iterations, pbkdf2KeyLen)
	secret := strings.TrimRight(base32.StdEncoding.EncodeToString(secretB), "=")
	u := User{
		Username:     username,
		Salt:         hex.EncodeToString(saltB),
		PasswordHash: hex.EncodeToString(hash),
		TOTPSecret:   secret,
		CreatedAt:    time.Now().Unix(),
	}
	return u, secret, totpURI("Pi Dashboard", username, secret), nil
}

func validUsername(s string) bool {
	if len(s) < 2 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}
	return true
}

// ---- Verification ----

func (s *AuthStore) VerifyPassword(username, password string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.findUser(username)
	if u == nil {
		// Run PBKDF2 anyway to keep timing consistent and avoid revealing user existence.
		pbkdf2([]byte(password), []byte("dummy"), pbkdf2Iterations, pbkdf2KeyLen)
		return false
	}
	salt, _ := hex.DecodeString(u.Salt)
	want, _ := hex.DecodeString(u.PasswordHash)
	got := pbkdf2([]byte(password), salt, pbkdf2Iterations, pbkdf2KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1
}

// Exists reports whether the username maps to a real account.
func (s *AuthStore) Exists(username string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.findUser(username) != nil
}

// HasTOTP reports whether the user exists and has a TOTP secret enrolled.
func (s *AuthStore) HasTOTP(username string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.findUser(username)
	return u != nil && u.TOTPSecret != ""
}

func (s *AuthStore) VerifyTOTP(username, code string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u := s.findUser(username)
	if u == nil {
		return false
	}
	return verifyTOTPSecret(u.TOTPSecret, code)
}

// verifyTOTPSecret checks a code against a raw secret (used by both verified
// users and pending users awaiting confirmation).
func verifyTOTPSecret(secret, code string) bool {
	// No secret enrolled means no code can ever be valid. Without this guard
	// an empty secret would still yield a deterministic (attacker-computable)
	// TOTP stream via HMAC with an empty key.
	if secret == "" {
		return false
	}
	now := time.Now().Unix() / totpPeriodSeconds
	for drift := -totpAllowedDrift; drift <= totpAllowedDrift; drift++ {
		if subtle.ConstantTimeCompare([]byte(totp(secret, now+int64(drift))), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

func (s *AuthStore) cookieSecret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, _ := hex.DecodeString(s.data.CookieSecret)
	return b
}

// ---- Session cookies (HMAC-signed, stateless) ----
// Format: <username>|<issuedAt>|<elevatedUntil>|<hmac-hex>

func (s *AuthStore) newCookie(username string, elevatedUntil time.Time) string {
	body := fmt.Sprintf("%s|%d|%d", username, time.Now().Unix(), elevatedUntil.Unix())
	mac := hmac.New(sha256.New, s.cookieSecret())
	mac.Write([]byte(body))
	return body + "|" + hex.EncodeToString(mac.Sum(nil))
}

type sessionInfo struct {
	Username      string
	IssuedAt      int64
	ElevatedUntil int64
}

func (s *AuthStore) parseCookie(raw string) (*sessionInfo, bool) {
	parts := strings.Split(raw, "|")
	if len(parts) != 4 {
		return nil, false
	}
	body := parts[0] + "|" + parts[1] + "|" + parts[2]
	wantSig, _ := hex.DecodeString(parts[3])
	mac := hmac.New(sha256.New, s.cookieSecret())
	mac.Write([]byte(body))
	if !hmac.Equal(mac.Sum(nil), wantSig) {
		return nil, false
	}
	issued, _ := strconv.ParseInt(parts[1], 10, 64)
	elev, _ := strconv.ParseInt(parts[2], 10, 64)
	if time.Since(time.Unix(issued, 0)) > sessionLifetime {
		return nil, false
	}
	// Username must still exist (don't honor cookies for deleted users).
	s.mu.RLock()
	exists := s.findUser(parts[0]) != nil
	s.mu.RUnlock()
	if !exists {
		return nil, false
	}
	return &sessionInfo{Username: parts[0], IssuedAt: issued, ElevatedUntil: elev}, true
}

func (s *AuthStore) sessionFrom(r *http.Request) (*sessionInfo, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil, false
	}
	return s.parseCookie(c.Value)
}

func setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionLifetime.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
}

// ---- Middleware ----

// bearerToken extracts an `Authorization: Bearer …` token, if present.
func bearerToken(r *http.Request) string {
	v := r.Header.Get("Authorization")
	if !strings.HasPrefix(v, "Bearer ") {
		return ""
	}
	return strings.TrimPrefix(v, "Bearer ")
}

// principalKey carries the authenticated username from the auth wrappers to
// the handlers.
//
// Needed because only ONE of the two mechanisms leaves a trace the handler can
// read: a session cookie is re-parsed via sessionFrom, but a bearer token is
// verified inside requireAuth and the username was then discarded. Handlers
// audit with sessionUser(sessionFrom(req)), so every API-token-authenticated
// action was recorded with an empty user.
type principalKey struct{}

func withPrincipal(r *http.Request, user string) *http.Request {
	if user == "" {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), principalKey{}, user))
}

// principalFrom is the authenticated username, or "" when the request was not
// authenticated by a mechanism that recorded one.
func principalFrom(r *http.Request) string {
	if r == nil {
		return ""
	}
	v, _ := r.Context().Value(principalKey{}).(string)
	return v
}

func (s *AuthStore) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.IsSetup() {
			http.Error(w, "auth not set up", http.StatusServiceUnavailable)
			return
		}
		if info, ok := s.sessionFrom(r); ok {
			next(w, withPrincipal(r, info.Username))
			return
		}
		if tok := bearerToken(r); tok != "" {
			if user := s.VerifyToken(tok); user != "" {
				next(w, withPrincipal(r, user))
				return
			}
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

func (s *AuthStore) requireElevated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.IsSetup() {
			http.Error(w, "auth not set up", http.StatusServiceUnavailable)
			return
		}
		// API tokens grant elevation (proof of possession of the token) —
		// except auto-provisioned service tokens, which VerifyElevatedToken
		// deliberately rejects (see RemintServiceToken).
		if tok := bearerToken(r); tok != "" {
			if user := s.VerifyElevatedToken(tok); user != "" {
				next(w, withPrincipal(r, user))
				return
			}
		}
		info, ok := s.sessionFrom(r)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if time.Now().Unix() > info.ElevatedUntil {
			http.Error(w, "2fa required", http.StatusForbidden)
			return
		}
		next(w, withPrincipal(r, info.Username))
	}
}

// ---- Crypto primitives ----

func pbkdf2(password, salt []byte, iter, keyLen int) []byte {
	hLen := sha256.Size
	numBlocks := (keyLen + hLen - 1) / hLen
	out := make([]byte, 0, numBlocks*hLen)
	buf := make([]byte, 4)
	for i := 1; i <= numBlocks; i++ {
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(i))
		mac.Write(buf)
		t := mac.Sum(nil)
		u := append([]byte(nil), t...)
		for j := 2; j <= iter; j++ {
			mac.Reset()
			mac.Write(u)
			u = mac.Sum(nil)
			for k := range t {
				t[k] ^= u[k]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func totp(secretB32 string, counter int64) string {
	pad := len(secretB32) % 8
	if pad != 0 {
		secretB32 += strings.Repeat("=", 8-pad)
	}
	key, err := base32.StdEncoding.DecodeString(strings.ToUpper(secretB32))
	if err != nil {
		return ""
	}
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	h := mac.Sum(nil)
	offset := int(h[len(h)-1] & 0x0f)
	code := (uint32(h[offset]&0x7f) << 24) |
		(uint32(h[offset+1]) << 16) |
		(uint32(h[offset+2]) << 8) |
		uint32(h[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, code%mod)
}

func totpURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	q := url.Values{}
	q.Set("secret", secret)
	q.Set("issuer", issuer)
	q.Set("algorithm", "SHA1")
	q.Set("digits", strconv.Itoa(totpDigits))
	q.Set("period", strconv.Itoa(totpPeriodSeconds))
	return "otpauth://totp/" + label + "?" + q.Encode()
}
