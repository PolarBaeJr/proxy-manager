package sso

import (
	"strings"
	"testing"
	"time"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func futureExp() int64 { return time.Now().Add(time.Hour).Unix() }

// flipChar swaps the base64url character at index i for a different one,
// avoiding the final character of a segment where non-canonical trailing
// bits could decode to the same bytes.
func flipChar(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'Q'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

func TestClientRoundTrip(t *testing.T) {
	in := ClientReg{RedirectURIs: []string{"https://claude.ai/api/mcp/auth_callback"}, Name: "Claude", IssuedAt: time.Now().Unix()}
	blob := SignClient(in, testSecret)
	if !strings.HasPrefix(blob, ClientIDPrefix) {
		t.Fatalf("blob %q lacks prefix %q", blob, ClientIDPrefix)
	}
	out, ok := VerifyClient(blob, testSecret)
	if !ok {
		t.Fatal("VerifyClient rejected its own blob")
	}
	if out.Name != in.Name || out.IssuedAt != in.IssuedAt || len(out.RedirectURIs) != 1 || out.RedirectURIs[0] != in.RedirectURIs[0] {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
	if _, ok := VerifyClient(blob, []byte("wrong-secret")); ok {
		t.Fatal("VerifyClient accepted blob under wrong secret")
	}
}

func TestCodeRoundTrip(t *testing.T) {
	in := CodeClaims{
		Username: "alice", ClientID: "pmgcl_x", Audience: "mcp.polardev.org", Scope: "mcp",
		RedirectURI: "https://claude.ai/cb", CodeChallenge: "abc123", JTI: "deadbeef", Exp: futureExp(),
	}
	blob := SignCode(in, testSecret)
	out, ok := VerifyCode(blob, testSecret)
	if !ok {
		t.Fatal("VerifyCode rejected its own blob")
	}
	if out != in {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestAccessRoundTrip(t *testing.T) {
	in := AccessClaims{Username: "alice", ClientID: "pmgcl_x", Audience: "*", Scope: "", Exp: futureExp()}
	blob := SignAccess(in, testSecret)
	out, ok := VerifyAccess(blob, testSecret)
	if !ok {
		t.Fatal("VerifyAccess rejected its own blob")
	}
	if out != in {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestRefreshRoundTrip(t *testing.T) {
	in := RefreshClaims{Username: "alice", ClientID: "pmgcl_x", Audience: "mcp.polardev.org", Scope: "mcp", JTI: "cafebabe", Exp: futureExp()}
	blob := SignRefresh(in, testSecret)
	out, ok := VerifyRefresh(blob, testSecret)
	if !ok {
		t.Fatal("VerifyRefresh rejected its own blob")
	}
	if out != in {
		t.Fatalf("round trip mismatch: %+v != %+v", out, in)
	}
}

func TestTamperedPayloadRejected(t *testing.T) {
	blob := SignAccess(AccessClaims{Username: "alice", Audience: "*", Exp: futureExp()}, testSecret)
	// A couple of characters into the payload segment.
	if _, ok := VerifyAccess(flipChar(blob, len(AccessTokenPrefix)+2), testSecret); ok {
		t.Fatal("VerifyAccess accepted tampered payload")
	}
}

func TestTamperedMACRejected(t *testing.T) {
	blob := SignAccess(AccessClaims{Username: "alice", Audience: "*", Exp: futureExp()}, testSecret)
	dot := strings.LastIndexByte(blob, '.')
	if dot < 0 {
		t.Fatal("blob has no mac segment")
	}
	// A couple of characters into the mac segment (not the final char, whose
	// low bits are base64 padding).
	if _, ok := VerifyAccess(flipChar(blob, dot+2), testSecret); ok {
		t.Fatal("VerifyAccess accepted tampered mac")
	}
}

func TestExpiredRejected(t *testing.T) {
	past := time.Now().Add(-time.Minute).Unix()
	if _, ok := VerifyCode(SignCode(CodeClaims{Username: "alice", Exp: past}, testSecret), testSecret); ok {
		t.Fatal("VerifyCode accepted expired code")
	}
	if _, ok := VerifyAccess(SignAccess(AccessClaims{Username: "alice", Exp: past}, testSecret), testSecret); ok {
		t.Fatal("VerifyAccess accepted expired token")
	}
	if _, ok := VerifyRefresh(SignRefresh(RefreshClaims{Username: "alice", Exp: past}, testSecret), testSecret); ok {
		t.Fatal("VerifyRefresh accepted expired token")
	}
}

// Tag separation: an access blob must not verify as a refresh token even if
// an attacker rewrites the prefix — the MAC covers the per-type tag, not the
// prefix, so this is the check that matters.
func TestCrossTypeConfusionRejected(t *testing.T) {
	access := SignAccess(AccessClaims{Username: "alice", ClientID: "c", Audience: "*", Exp: futureExp()}, testSecret)
	if _, ok := VerifyRefresh(access, testSecret); ok {
		t.Fatal("VerifyRefresh accepted an access blob")
	}
	forged := RefreshTokenPrefix + strings.TrimPrefix(access, AccessTokenPrefix)
	if _, ok := VerifyRefresh(forged, testSecret); ok {
		t.Fatal("VerifyRefresh accepted a prefix-swapped access blob")
	}
	code := SignCode(CodeClaims{Username: "alice", Exp: futureExp()}, testSecret)
	forgedAccess := AccessTokenPrefix + strings.TrimPrefix(code, AuthCodePrefix)
	if _, ok := VerifyAccess(forgedAccess, testSecret); ok {
		t.Fatal("VerifyAccess accepted a prefix-swapped code blob")
	}
}

func TestSSOCookieIsNotABlob(t *testing.T) {
	cookie := Sign("alice", time.Now().Add(time.Hour), testSecret)
	if _, ok := VerifyClient(cookie, testSecret); ok {
		t.Fatal("VerifyClient accepted an sso cookie value")
	}
	if _, ok := VerifyCode(cookie, testSecret); ok {
		t.Fatal("VerifyCode accepted an sso cookie value")
	}
	if _, ok := VerifyAccess(cookie, testSecret); ok {
		t.Fatal("VerifyAccess accepted an sso cookie value")
	}
	if _, ok := VerifyRefresh(cookie, testSecret); ok {
		t.Fatal("VerifyRefresh accepted an sso cookie value")
	}
}
