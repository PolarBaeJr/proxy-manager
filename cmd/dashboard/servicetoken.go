package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// provisionServiceToken ensures a service identity (e.g. "statusbot") has a
// live credential written to <dir>/<name>.token for a sibling container to
// read. A token's raw value can only ever be shown once, so this can't just
// re-derive whether the file is still good — it validates the file's
// contents against the AuthStore (VerifyToken) rather than merely checking
// the file exists. That makes it self-heal in both directions: a missing
// file (first boot, or an operator deleted it to force rotation) mints
// fresh, and so does a file whose credential no longer matches the store
// (e.g. auth.json got replaced/restored out from under it) — otherwise
// statusbot would ship a dead token forever with no visible symptom besides
// 401s. A file that still verifies is left untouched, even across restarts.
//
// dir is a small, DEDICATED mount — never the general /data volume that also
// holds auth.json/audit.log/onboarded.json/etc — so a sibling container only
// ever sees this one credential file, nothing else about dashboard state.
func provisionServiceToken(auth *AuthStore, dir, name string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("service token dir: %w", err)
	}
	path := filepath.Join(dir, name+".token")
	if b, err := os.ReadFile(path); err == nil {
		if auth.VerifyToken(strings.TrimSpace(string(b))) == name {
			log.Printf("service token for %q already provisioned at %s", name, path)
			return nil
		}
		log.Printf("service token file at %s no longer matches a valid credential for %q — reminting", path, name)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}

	raw, err := auth.RemintServiceToken(name)
	if err != nil {
		return fmt.Errorf("mint token for %q: %w", name, err)
	}
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	log.Printf("minted and wrote a fresh service token for %q to %s", name, path)
	return nil
}
