// Package webauthn holds the RP-agnostic WebAuthn / passkey plumbing shared by
// the dashboard-style credential flows and the multi-tenant SSO portal. It
// wraps github.com/go-webauthn/webauthn with an in-memory ceremony store and a
// small User adapter so callers can plug their own credential storage.
//
// Sessions for in-flight begin/finish pairs live in an in-memory map with a
// short TTL so a hung browser doesn't pin server memory. We don't persist them
// because both halves of a registration or login complete within seconds.
package webauthn

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

// StoredCredential is the persisted shape of one passkey for a user.
type StoredCredential struct {
	ID         []byte   `json:"id"`         // raw credential ID
	PublicKey  []byte   `json:"public_key"` // COSE-encoded public key
	AAGUID     []byte   `json:"aaguid,omitempty"`
	SignCount  uint32   `json:"sign_count"`
	Label      string   `json:"label"` // user-supplied nickname
	Transports []string `json:"transports,omitempty"`
	CreatedAt  int64    `json:"created_at"`
	LastUsedAt int64    `json:"last_used_at,omitempty"`

	// Flags tracked by go-webauthn. The library refuses validation if a stored
	// credential reports BackupEligible=false but the response says true (or
	// vice versa) — Apple flips BE the moment iCloud Keychain backs up the
	// passkey, so we MUST persist these to avoid bogus rejections.
	UserPresent    bool `json:"flag_up,omitempty"`
	UserVerified   bool `json:"flag_uv,omitempty"`
	BackupEligible bool `json:"flag_be,omitempty"`
	BackupState    bool `json:"flag_bs,omitempty"`
}

// ToCredential converts a StoredCredential to the library's runtime shape.
func (c StoredCredential) ToCredential() webauthn.Credential {
	return webauthn.Credential{
		ID:        c.ID,
		PublicKey: c.PublicKey,
		Authenticator: webauthn.Authenticator{
			AAGUID:    c.AAGUID,
			SignCount: c.SignCount,
		},
		Flags: webauthn.CredentialFlags{
			UserPresent:    c.UserPresent,
			UserVerified:   c.UserVerified,
			BackupEligible: c.BackupEligible,
			BackupState:    c.BackupState,
		},
	}
}

// Credentials converts a slice of stored credentials to library credentials.
func Credentials(stored []StoredCredential) []webauthn.Credential {
	out := make([]webauthn.Credential, 0, len(stored))
	for _, c := range stored {
		out = append(out, c.ToCredential())
	}
	return out
}

// UserID derives a stable 32-byte WebAuthn user handle from a username. HMAC
// isn't needed — the handle only has to be stable and opaque, so we embed the
// lowercased username in a fixed prefix and pad to 32 bytes. Registration
// (which writes the handle) and login (which matches it) MUST call this same
// function or discoverable login can't recover the user.
func UserID(username string) []byte {
	id := make([]byte, 32)
	h := []byte("webauthn-id:" + strings.ToLower(username))
	copy(id, h)
	if len(h) < 32 {
		for i := len(h); i < 32; i++ {
			id[i] = 0
		}
	}
	return id
}

// Adapter implements webauthn.User over a caller-supplied id, name, and
// credential set — the library reads only these four methods.
type Adapter struct {
	id    []byte
	name  string
	creds []webauthn.Credential
}

// NewAdapter builds a webauthn.User from raw parts.
func NewAdapter(id []byte, name string, creds []webauthn.Credential) *Adapter {
	return &Adapter{id: id, name: name, creds: creds}
}

func (u *Adapter) WebAuthnID() []byte                         { return u.id }
func (u *Adapter) WebAuthnName() string                       { return u.name }
func (u *Adapter) WebAuthnDisplayName() string                { return u.name }
func (u *Adapter) WebAuthnCredentials() []webauthn.Credential { return u.creds }

// PendingCeremony is one in-flight begin/finish pair.
type PendingCeremony struct {
	Session  webauthn.SessionData
	Username string // empty for discoverable / passkey-first login
	Expires  time.Time
}

const ceremonyTTL = 5 * time.Minute

// Manager wraps the webauthn library and tracks in-flight ceremonies for one
// relying party (one RP ID / origin set).
type Manager struct {
	W   *webauthn.WebAuthn
	mu  sync.Mutex
	regs map[string]*PendingCeremony // keyed by ceremony token
}

// NewManager builds a Manager for the given RP ID, origins, and display name.
func NewManager(rpID string, origins []string, display string) (*Manager, error) {
	w, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: display,
		RPOrigins:     origins,
	})
	if err != nil {
		return nil, err
	}
	return &Manager{W: w, regs: map[string]*PendingCeremony{}}, nil
}

// PutCeremony stores a ceremony under a fresh random token (5-min TTL) and
// opportunistically GCs expired entries.
func (m *Manager) PutCeremony(c *PendingCeremony) (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	tok := base64.RawURLEncoding.EncodeToString(b)
	c.Expires = time.Now().Add(ceremonyTTL)
	m.mu.Lock()
	m.regs[tok] = c
	for k, v := range m.regs {
		if time.Now().After(v.Expires) {
			delete(m.regs, k)
		}
	}
	m.mu.Unlock()
	return tok, nil
}

// PopCeremony removes and returns the ceremony for tok, or false if it's
// missing or expired.
func (m *Manager) PopCeremony(tok string) (*PendingCeremony, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.regs[tok]
	if !ok {
		return nil, false
	}
	delete(m.regs, tok)
	if time.Now().After(c.Expires) {
		return nil, false
	}
	return c, true
}

// CredIDKey is the stable string key identifying a credential in the API: the
// hex of the raw credential ID.
func CredIDKey(id []byte) string { return hex.EncodeToString(id) }
