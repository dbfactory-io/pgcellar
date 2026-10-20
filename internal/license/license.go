// Package license reads and checks pgCellar licence keys. A key is signed
// with Ed25519 and verified offline against a public key built into the
// binary: no account, no phone-home.
//
// Product rule: a licence never blocks a backup, a restore or a drill. It only
// limits how many servers can be ADDED.
//
// Key format: PGC1.<base64url(payload JSON)>.<base64url(signature)>, the
// signature covering the base64url payload text exactly as it appears.
package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// Prefix opens every key; the digit is the format version.
const Prefix = "PGC1"

// URL is where the editions are sold; it appears in the messages.
const URL = "https://dbfactory.io/en/pgcellar/"

// Editions.
const (
	Free     = "free"
	Pro      = "pro"
	Business = "business"
)

// FreeServers is the number of servers the Free edition can hold.
const FreeServers = 2

// Grace is how long a licence keeps working after it expired.
const Grace = 30 * 24 * time.Hour

const dateLayout = "2006-01-02"

// Payload is what a key says. Dates are plain days (YYYY-MM-DD); a licence is
// valid through the end of its expiry day (UTC).
type Payload struct {
	ID       string `json:"id"`
	Edition  string `json:"edition"`
	Licensee string `json:"licensee"`
	Servers  int    `json:"servers"` // 0 = unlimited
	Issued   string `json:"issued"`
	Expires  string `json:"expires"`
}

// States of a licence.
const (
	StateFree    = "free"    // no key
	StateValid   = "valid"   // signed, not expired
	StateGrace   = "grace"   // expired less than Grace ago: still honoured
	StateExpired = "expired" // expired for good: same limits as Free
	StateInvalid = "invalid" // the key does not verify: same limits as Free
)

// Status is what the application knows about its licence.
type Status struct {
	State    string   `json:"state"`
	Edition  string   `json:"edition"`
	Licensee string   `json:"licensee,omitempty"`
	ID       string   `json:"id,omitempty"`
	Servers  int      `json:"servers"` // servers the key allows, 0 = unlimited (of the edition in force)
	Issued   string   `json:"issued,omitempty"`
	Expires  string   `json:"expires,omitempty"`
	GraceEnd string   `json:"grace_end,omitempty"`
	Message  string   `json:"message,omitempty"`
	Payload  *Payload `json:"-"`
}

// Max is the number of servers that can be held now: 0 means unlimited. A
// licence that is expired for good, or invalid, falls back to Free.
func (s Status) Max() int {
	switch s.State {
	case StateValid, StateGrace:
		return s.Servers
	}
	return FreeServers
}

// Unlimited reports whether any number of servers may be added.
func (s Status) Unlimited() bool { return s.Max() == 0 }

// Allows reports whether one more server can be added when `have` exist.
func (s Status) Allows(have int) bool { return s.Unlimited() || have < s.Max() }

// AddError is the sentence shown when a server cannot be added, "" when it can.
func (s Status) AddError(have int) string {
	if s.Allows(have) {
		return ""
	}
	why := fmt.Sprintf("The %s edition holds %d servers", s.EditionName(), s.Max())
	switch s.State {
	case StateExpired:
		why = fmt.Sprintf("Your licence expired on %s and pgCellar is back to the Free edition (%d servers)", s.Expires, FreeServers)
	case StateInvalid:
		why = fmt.Sprintf("The licence key is not valid and pgCellar is on the Free edition (%d servers)", FreeServers)
	}
	return fmt.Sprintf("%s, and you already have %d. Backups, restores and drills of existing servers keep working. To add more servers, get a licence at %s.", why, have, URL)
}

// EditionName is "Free", "Pro" or "Business".
func (s Status) EditionName() string {
	e := s.Edition
	if s.State == StateExpired || s.State == StateInvalid {
		e = Free
	}
	if e == "" {
		e = Free
	}
	return strings.ToUpper(e[:1]) + e[1:]
}

// Footer is the discreet line of the page footer: "Free edition · 2 of 2 servers".
func (s Status) Footer(have int) string {
	out := s.EditionName() + " edition"
	if s.Licensee != "" && (s.State == StateValid || s.State == StateGrace) {
		out += " · " + s.Licensee
	}
	if s.Unlimited() {
		out += fmt.Sprintf(" · %d servers", have)
	} else {
		out += fmt.Sprintf(" · %d of %d servers", have, s.Max())
	}
	return out
}

// Banner is the sentence of the discreet banner on the Status page, "" when
// there is nothing to say.
func (s Status) Banner(have int) string {
	switch {
	case s.State == StateGrace:
		return fmt.Sprintf("Your %s licence expired on %s. It keeps working until %s; renew it at %s.", s.EditionName(), s.Expires, s.GraceEnd, URL)
	case s.State == StateExpired:
		return fmt.Sprintf("Your licence expired on %s: pgCellar is on the Free edition (%d servers). Backups keep running; renew at %s to add servers.", s.Expires, FreeServers, URL)
	case s.State == StateInvalid:
		return fmt.Sprintf("The licence key is not valid (%s). pgCellar is on the Free edition (%d servers). Backups keep running.", s.Message, FreeServers)
	case !s.Unlimited() && have > s.Max():
		return fmt.Sprintf("This installation holds %d servers, more than the %d of the %s edition. Everything keeps running; you cannot add servers until you get a licence at %s.", have, s.Max(), s.EditionName(), URL)
	}
	return ""
}

// ---- keys ----

// GenerateKeyPair makes a signing key pair.
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	return ed25519.GenerateKey(rand.Reader)
}

// EncodeKey writes a key (public or private) as base64 text.
func EncodeKey(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// DecodePublic reads a base64 public key.
func DecodePublic(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("not a public key")
	}
	return ed25519.PublicKey(b), nil
}

// DecodePrivate reads a base64 private key.
func DecodePrivate(s string) (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errors.New("not a private key")
	}
	return ed25519.PrivateKey(b), nil
}

// ReadPrivateFile reads a signing key written by `pgcellar license keygen`.
func ReadPrivateFile(path string) (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return DecodePrivate(string(b))
}

var b64 = base64.RawURLEncoding

// Sign builds the key text for a payload.
func Sign(priv ed25519.PrivateKey, p Payload) (string, error) {
	if err := p.check(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	body := b64.EncodeToString(raw)
	sig := ed25519.Sign(priv, []byte(Prefix+"."+body))
	return Prefix + "." + body + "." + b64.EncodeToString(sig), nil
}

func (p Payload) check() error {
	switch p.Edition {
	case Free, Pro, Business:
	default:
		return fmt.Errorf("edition %q is not free, pro or business", p.Edition)
	}
	if p.Servers < 0 {
		return errors.New("servers cannot be negative (0 = unlimited)")
	}
	if p.ID == "" {
		return errors.New("the licence has no id")
	}
	if _, err := time.Parse(dateLayout, p.Expires); err != nil {
		return fmt.Errorf("expires %q is not a date (YYYY-MM-DD)", p.Expires)
	}
	if _, err := time.Parse(dateLayout, p.Issued); err != nil {
		return fmt.Errorf("issued %q is not a date (YYYY-MM-DD)", p.Issued)
	}
	return nil
}

// Parse checks the signature of a key against pub and returns its payload.
// It does not look at dates.
func Parse(key string, pub ed25519.PublicKey) (*Payload, error) {
	if len(pub) != ed25519.PublicKeySize {
		return nil, errors.New("this build has no licence verification key")
	}
	key = strings.TrimSpace(key)
	parts := strings.Split(key, ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return nil, errors.New("this is not a pgCellar licence key (it should look like PGC1.xxxx.yyyy)")
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, errors.New("the signature part of the key is damaged")
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, errors.New("the signature does not match: the key was altered or was not issued by dbfactory")
	}
	raw, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, errors.New("the payload part of the key is damaged")
	}
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("the payload part of the key is damaged")
	}
	if err := p.check(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Evaluate turns a key (maybe empty) into the status at `now`.
func Evaluate(key string, pub ed25519.PublicKey, now time.Time) Status {
	if strings.TrimSpace(key) == "" {
		return Status{State: StateFree, Edition: Free, Servers: FreeServers}
	}
	p, err := Parse(key, pub)
	if err != nil {
		return Status{State: StateInvalid, Edition: Free, Servers: FreeServers, Message: err.Error()}
	}
	st := Status{State: StateValid, Edition: p.Edition, Licensee: p.Licensee, ID: p.ID, Servers: p.Servers, Issued: p.Issued, Expires: p.Expires, Payload: p}
	if p.Edition == Free && p.Servers == 0 {
		st.Servers = FreeServers // a "free" key never lifts the cap
	}
	end, _ := time.Parse(dateLayout, p.Expires)
	end = end.AddDate(0, 0, 1) // valid through the whole expiry day
	st.GraceEnd = end.Add(Grace).Add(-time.Second).Format(dateLayout)
	switch {
	case now.Before(end):
	case now.Before(end.Add(Grace)):
		st.State = StateGrace
	default:
		st.State = StateExpired
	}
	return st
}

// KeyText returns the key text of the configuration: the key itself, or the
// content of the file. "" when neither is set. An unreadable file is an error.
func KeyText(key, file string) (string, error) {
	if k := strings.TrimSpace(key); k != "" {
		return k, nil
	}
	if strings.TrimSpace(file) == "" {
		return "", nil
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return "", fmt.Errorf("cannot read license.file: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}
