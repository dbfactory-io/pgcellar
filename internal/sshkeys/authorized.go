package sshkeys

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/stranix79/pgcellar/internal/barman"
)

// authorizedName is the file the SSH companion container reads (sshd_config:
// AuthorizedKeysFile). The file is the truth: nothing about it is cached.
const authorizedName = "authorized_keys"

// commentPrefix marks, in the comment of a line, the Barman server the key
// was authorised for.
const commentPrefix = "pgcellar:"

// AuthorizedPath is the authorized_keys file of a home directory.
func AuthorizedPath(home string) string { return filepath.Join(Dir(home), authorizedName) }

// AuthorizedKey is one key allowed to push WAL files.
type AuthorizedKey struct {
	Server      string `json:"server"`      // Barman server named in the forced command
	Type        string `json:"type"`        // ssh-ed25519, ssh-rsa, ecdsa-sha2-nistp256...
	Fingerprint string `json:"fingerprint"` // SHA256:... (what ssh-keygen -l prints)
	Comment     string `json:"comment"`
	Managed     bool   `json:"managed"` // the line has the restriction written by pgCellar
}

// ForcedCommand is the only command a key of a PostgreSQL server may run:
// barman-wal-archive pipes a tar of the WAL file into `barman put-wal <server>`.
func ForcedCommand(server string) string { return "barman put-wal " + server }

// ParsePublicKey checks a public key pasted by a person and returns its
// normalised "<type> <base64>" form and its fingerprint. It must be exactly one
// line, without options, of type ssh-ed25519, ssh-rsa or ecdsa-sha2-*.
func ParsePublicKey(text string) (line, fingerprint string, err error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", "", errors.New("the public key is empty")
	}
	if strings.ContainsAny(text, "\r\n") {
		return "", "", errors.New("the public key must be one single line")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		return "", "", errors.New("this is a private key: paste the public key (the .pub file)")
	}
	key, _, opts, _, perr := ssh.ParseAuthorizedKey([]byte(text))
	if perr != nil {
		return "", "", errors.New("not a valid public key: expected one line like \"ssh-ed25519 AAAA... comment\"")
	}
	if len(opts) > 0 {
		return "", "", errors.New("the public key must not carry options: paste the line of the .pub file as it is")
	}
	t := key.Type()
	if t != ssh.KeyAlgoED25519 && t != ssh.KeyAlgoRSA && !strings.HasPrefix(t, "ecdsa-sha2-") {
		return "", "", fmt.Errorf("key type %s is not accepted: use ssh-ed25519, ssh-rsa or ecdsa", t)
	}
	line = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	return line, ssh.FingerprintSHA256(key), nil
}

// parsedLine is one line of the file.
type parsedLine struct {
	raw string
	key *AuthorizedKey // nil for comments, blank lines and anything not understood
}

func readLines(home string) ([]parsedLine, error) {
	f, err := os.Open(AuthorizedPath(home))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []parsedLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		raw := sc.Text()
		pl := parsedLine{raw: raw}
		if t := strings.TrimSpace(raw); t != "" && !strings.HasPrefix(t, "#") {
			if key, comment, opts, _, perr := ssh.ParseAuthorizedKey([]byte(raw)); perr == nil {
				k := &AuthorizedKey{Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key), Comment: comment}
				k.Server = strings.TrimPrefix(comment, commentPrefix)
				if !strings.HasPrefix(comment, commentPrefix) {
					k.Server = ""
				}
				k.Managed = k.Server != "" && hasOption(opts, "restrict") && hasOption(opts, `command="`+ForcedCommand(k.Server)+`"`)
				pl.key = k
			}
		}
		out = append(out, pl)
	}
	return out, sc.Err()
}

func hasOption(opts []string, want string) bool {
	for _, o := range opts {
		if o == want {
			return true
		}
	}
	return false
}

// ListAuthorized reads the file and returns the keys it holds, in file order.
// A missing file is an empty list.
func ListAuthorized(home string) ([]AuthorizedKey, error) {
	lines, err := readLines(home)
	if err != nil {
		return nil, err
	}
	var out []AuthorizedKey
	for _, l := range lines {
		if l.key != nil {
			out = append(out, *l.key)
		}
	}
	return out, nil
}

// ErrDuplicate means the key is already in the file.
var ErrDuplicate = errors.New("this key is already authorised")

// ErrNoSuchKey means no line has that fingerprint.
var ErrNoSuchKey = errors.New("no such key")

// Allow appends the key of a PostgreSQL server, restricted to
// `barman put-wal <server>` (no pty, no forwarding, no other command). The file
// and ~/.ssh are created with 0600 / 0700 when absent. A key already present
// (same fingerprint, whatever its server) is refused with ErrDuplicate.
func Allow(home, server, publicKey string) (fingerprint string, err error) {
	if !barman.ValidName(server) {
		return "", fmt.Errorf("invalid server name %q", server)
	}
	line, fp, err := ParsePublicKey(publicKey)
	if err != nil {
		return "", err
	}
	lines, err := readLines(home)
	if err != nil {
		return "", err
	}
	for _, l := range lines {
		if l.key != nil && l.key.Fingerprint == fp {
			return "", ErrDuplicate
		}
	}
	raws := make([]string, 0, len(lines)+1)
	for _, l := range lines {
		raws = append(raws, l.raw)
	}
	raws = append(raws, `restrict,command="`+ForcedCommand(server)+`" `+line+" "+commentPrefix+server)
	return fp, writeLines(home, raws)
}

// Revoke removes the line whose key has this fingerprint. Other lines,
// comments included, stay as they are.
func Revoke(home, fingerprint string) (AuthorizedKey, error) {
	lines, err := readLines(home)
	if err != nil {
		return AuthorizedKey{}, err
	}
	var removed *AuthorizedKey
	raws := make([]string, 0, len(lines))
	for _, l := range lines {
		if removed == nil && l.key != nil && l.key.Fingerprint == fingerprint {
			k := *l.key
			removed = &k
			continue
		}
		raws = append(raws, l.raw)
	}
	if removed == nil {
		return AuthorizedKey{}, ErrNoSuchKey
	}
	return *removed, writeLines(home, raws)
}

// writeLines replaces the file atomically (temporary file in the same
// directory, mode 0600, then rename).
func writeLines(home string, raws []string) error {
	dir := Dir(home)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, authorizedName+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	var b strings.Builder
	for _, r := range raws {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	if _, err := tmp.WriteString(b.String()); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, AuthorizedPath(home)); err != nil {
		return err
	}
	ok = true
	return nil
}
