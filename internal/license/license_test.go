package license

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func pair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func issue(t *testing.T, priv ed25519.PrivateKey, edition string, servers int, expires string) string {
	t.Helper()
	k, err := Sign(priv, Payload{ID: "lic-1", Edition: edition, Licensee: "ACME SA", Servers: servers, Issued: "2026-10-01", Expires: expires})
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func day(s string) time.Time {
	d, _ := time.Parse(dateLayout, s)
	return d.Add(12 * time.Hour)
}

func TestSignatureValidInvalidAltered(t *testing.T) {
	pub, priv := pair(t)
	key := issue(t, priv, Pro, 10, "2027-12-31")
	if !strings.HasPrefix(key, "PGC1.") || strings.Count(key, ".") != 2 {
		t.Fatalf("format: %s", key)
	}
	p, err := Parse(key, pub)
	if err != nil || p.Licensee != "ACME SA" || p.Servers != 10 || p.Edition != Pro {
		t.Fatalf("parse: %+v %v", p, err)
	}
	// another issuer
	other, _ := pair(t)
	if _, err := Parse(key, other); err == nil {
		t.Error("a key signed by someone else must not verify")
	}
	// altered payload: same signature, other body
	_, priv2 := pair(t)
	forged := issue(t, priv2, Business, 0, "2030-01-01")
	parts := strings.Split(key, ".")
	fparts := strings.Split(forged, ".")
	if _, err := Parse(parts[0]+"."+fparts[1]+"."+parts[2], pub); err == nil {
		t.Error("an altered payload must not verify")
	}
	// altered signature
	sig := []byte(parts[2])
	if sig[3] == 'A' {
		sig[3] = 'B'
	} else {
		sig[3] = 'A'
	}
	if _, err := Parse(parts[0]+"."+parts[1]+"."+string(sig), pub); err == nil {
		t.Error("an altered signature must not verify")
	}
	for _, bad := range []string{"", "nonsense", "PGC1.a.b", "PGC2." + parts[1] + "." + parts[2]} {
		if _, err := Parse(bad, pub); err == nil {
			t.Errorf("%q must not verify", bad)
		}
	}
	if st := Evaluate(key+"x", pub, day("2026-11-01")); st.State != StateInvalid || st.Max() != FreeServers {
		t.Errorf("invalid key: %+v", st)
	}
}

func TestExpiryGraceAndQuota(t *testing.T) {
	pub, priv := pair(t)
	key := issue(t, priv, Pro, 10, "2026-12-31")

	st := Evaluate(key, pub, day("2026-12-31"))
	if st.State != StateValid || st.Max() != 10 || !st.Allows(9) || st.Allows(10) {
		t.Errorf("valid on the last day: %+v", st)
	}
	st = Evaluate(key, pub, day("2027-01-15"))
	if st.State != StateGrace || st.Max() != 10 || st.Banner(3) == "" || st.GraceEnd != "2027-01-30" {
		t.Errorf("grace: %+v / %q", st, st.Banner(3))
	}
	st = Evaluate(key, pub, day("2027-01-31"))
	if st.State != StateExpired || st.Max() != FreeServers || st.Allows(2) || !st.Allows(1) {
		t.Errorf("expired: %+v", st)
	}
	if msg := st.AddError(2); !strings.Contains(msg, "dbfactory.io/pgcellar") || !strings.Contains(msg, "keep working") {
		t.Errorf("message: %q", msg)
	}
	if st.Banner(2) == "" {
		t.Error("an expired licence shows a banner")
	}

	free := Evaluate("", pub, day("2026-11-01"))
	if free.State != StateFree || free.Max() != 2 || free.Allows(2) || !free.Allows(1) || free.Banner(2) != "" {
		t.Errorf("free: %+v", free)
	}
	if free.Footer(2) != "Free edition · 2 of 2 servers" {
		t.Errorf("footer: %q", free.Footer(2))
	}
	if free.Banner(3) == "" {
		t.Error("beyond the quota a banner shows")
	}

	unl := Evaluate(issue(t, priv, Business, 0, "2027-12-31"), pub, day("2026-11-01"))
	if !unl.Unlimited() || !unl.Allows(500) || unl.AddError(500) != "" || unl.Footer(7) != "Business edition · ACME SA · 7 servers" {
		t.Errorf("unlimited: %+v %q", unl, unl.Footer(7))
	}
}

func TestSignRejectsBadPayload(t *testing.T) {
	_, priv := pair(t)
	for _, p := range []Payload{
		{ID: "x", Edition: "gold", Issued: "2026-10-01", Expires: "2027-01-01"},
		{ID: "x", Edition: Pro, Servers: -1, Issued: "2026-10-01", Expires: "2027-01-01"},
		{ID: "x", Edition: Pro, Issued: "2026-10-01", Expires: "tomorrow"},
		{Edition: Pro, Issued: "2026-10-01", Expires: "2027-01-01"},
	} {
		if _, err := Sign(priv, p); err == nil {
			t.Errorf("%+v must be refused", p)
		}
	}
}

func TestKeyText(t *testing.T) {
	if k, err := KeyText(" PGC1.a.b ", ""); err != nil || k != "PGC1.a.b" {
		t.Errorf("%q %v", k, err)
	}
	f := filepath.Join(t.TempDir(), "license.key")
	if err := os.WriteFile(f, []byte("PGC1.x.y\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k, err := KeyText("", f); err != nil || k != "PGC1.x.y" {
		t.Errorf("%q %v", k, err)
	}
	if _, err := KeyText("", f+".missing"); err == nil {
		t.Error("a missing file is an error")
	}
	if k, _ := KeyText("", ""); k != "" {
		t.Error("nothing set = no key")
	}
}

func TestEmbeddedKeyIsValid(t *testing.T) {
	if len(PublicKey()) != ed25519.PublicKeySize {
		t.Fatal("PublicKeyBase64 is not a valid Ed25519 public key")
	}
}
