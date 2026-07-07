// Passkey credential store for the SSO portal. Persisted to /data/passkeys.json
// following the dashboard's PrefsStore/OnboardedStore pattern (mutex + save()).
// Credentials are keyed by (cookie domain, lowercased username): a WebAuthn
// credential is bound to one RP ID, and each cookie domain is its own RP.
//
// Only opened when -passkey-rp-domains is non-empty — with the flag empty the
// portal never touches /data.

package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	iwebauthn "github.com/PolarBaeJr/proxy-manager/internal/webauthn"
	"github.com/go-webauthn/webauthn/webauthn"
)

type PasskeyStore struct {
	path string
	mu   sync.RWMutex
	data map[string]map[string][]iwebauthn.StoredCredential // domain → username(lower) → creds
}

func loadPasskeyStore(path string) (*PasskeyStore, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	s := &PasskeyStore{path: path, data: map[string]map[string][]iwebauthn.StoredCredential{}}
	b, err := os.ReadFile(path)
	if err == nil {
		_ = json.Unmarshal(b, &s.data)
		if s.data == nil {
			s.data = map[string]map[string][]iwebauthn.StoredCredential{}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return s, nil
}

func (s *PasskeyStore) save() error {
	b, _ := json.MarshalIndent(s.data, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}

func credIDKey(id []byte) string { return hex.EncodeToString(id) }

// Add appends a credential for (domain, username), defaulting the label and
// stamping CreatedAt.
func (s *PasskeyStore) Add(domain, username string, c iwebauthn.StoredCredential) error {
	if c.Label == "" {
		c.Label = "Passkey"
	}
	c.CreatedAt = time.Now().Unix()
	u := strings.ToLower(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	byUser := s.data[domain]
	if byUser == nil {
		byUser = map[string][]iwebauthn.StoredCredential{}
		s.data[domain] = byUser
	}
	byUser[u] = append(byUser[u], c)
	return s.save()
}

// PublicCredential is the JSON shape returned to the enroll UI — hides the
// public key and uses the hex ID so the frontend can pass it back in DELETE.
type PublicCredential struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at,omitempty"`
}

func (s *PasskeyStore) List(domain, username string) []PublicCredential {
	u := strings.ToLower(username)
	s.mu.RLock()
	defer s.mu.RUnlock()
	creds := s.data[domain][u]
	out := make([]PublicCredential, 0, len(creds))
	for _, c := range creds {
		out = append(out, PublicCredential{
			ID:         credIDKey(c.ID),
			Label:      c.Label,
			CreatedAt:  c.CreatedAt,
			LastUsedAt: c.LastUsedAt,
		})
	}
	return out
}

func (s *PasskeyStore) Delete(domain, username, idKey string) error {
	u := strings.ToLower(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	creds := s.data[domain][u]
	for i, c := range creds {
		if credIDKey(c.ID) == idKey {
			s.data[domain][u] = append(creds[:i], creds[i+1:]...)
			return s.save()
		}
	}
	return fmt.Errorf("credential not found")
}

// snapshotForDomain returns a copy of every user's credentials for one domain —
// used by the discoverable-login handler which walks them looking for the one
// whose WebAuthnID matches the assertion's userHandle.
func (s *PasskeyStore) snapshotForDomain(domain string) map[string][]iwebauthn.StoredCredential {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string][]iwebauthn.StoredCredential{}
	for u, creds := range s.data[domain] {
		cp := make([]iwebauthn.StoredCredential, len(creds))
		copy(cp, creds)
		out[u] = cp
	}
	return out
}

// listForDomain reports whether any user under the domain has a credential.
func (s *PasskeyStore) hasAnyForDomain(domain string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, creds := range s.data[domain] {
		if len(creds) > 0 {
			return true
		}
	}
	return false
}

// UpdateSignCount bumps SignCount + LastUsedAt + Flags for an existing
// credential after a successful login. Persisting the flags is what stops
// "BackupEligible inconsistency" errors when iCloud Keychain syncs the passkey.
func (s *PasskeyStore) UpdateSignCount(domain, username string, credID []byte, signCount uint32, flags webauthn.CredentialFlags) bool {
	u := strings.ToLower(username)
	s.mu.Lock()
	defer s.mu.Unlock()
	creds := s.data[domain][u]
	for i := range creds {
		if string(creds[i].ID) == string(credID) {
			creds[i].SignCount = signCount
			creds[i].UserPresent = flags.UserPresent
			creds[i].UserVerified = flags.UserVerified
			creds[i].BackupEligible = flags.BackupEligible
			creds[i].BackupState = flags.BackupState
			creds[i].LastUsedAt = time.Now().Unix()
			_ = s.save()
			return true
		}
	}
	return false
}
