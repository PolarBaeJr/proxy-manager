package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Append-only JSONL of every write action: who, what, when, from where.
// File lives next to auth.json on the mounted volume.

var (
	auditMu sync.Mutex
	auditF  *os.File
)

func openAuditLog(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	auditF = f
	return nil
}

func audit(r *http.Request, user, action, target string) {
	if auditF == nil {
		return
	}
	auditMu.Lock()
	defer auditMu.Unlock()
	fmt.Fprintf(auditF,
		`{"ts":%q,"user":%q,"action":%q,"target":%q,"ip":%q}`+"\n",
		time.Now().UTC().Format(time.RFC3339),
		user, action, target, clientIP(r),
	)
}
