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
	Username     string `json:"username"`
	Salt         string `json:"salt"`
	PasswordHash string `json:"password_hash"`
	TOTPSecret   string `json:"totp_secret"`
	CreatedAt    int64  `json:"created_at"`
}

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
	CookieSecret string `json:"cookie_secret"`
	Users        []User `json:"users"`

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

func (s *AuthStore) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.IsSetup() {
			http.Error(w, "auth not set up", http.StatusServiceUnavailable)
			return
		}
		if _, ok := s.sessionFrom(r); !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *AuthStore) requireElevated(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.IsSetup() {
			http.Error(w, "auth not set up", http.StatusServiceUnavailable)
			return
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
		next(w, r)
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
