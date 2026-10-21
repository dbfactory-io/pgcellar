package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/license"
)

// verifyKey is the key licences are checked against; tests replace it.
var verifyKey = license.PublicKey

// licenseCmd implements `pgcellar license keygen|issue|verify|show`.
func licenseCmd(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(stderr, "usage: pgcellar license keygen|issue|verify|show ...")
		return 2
	}
	switch args[0] {
	case "keygen":
		return licenseKeygen(args[1:], stdout, stderr)
	case "issue":
		return licenseIssue(args[1:], stdout, stderr)
	case "verify":
		return licenseVerify(args[1:], stdout, stderr)
	case "show":
		return licenseShow(args[1:], stdout, stderr)
	}
	_, _ = fmt.Fprintf(stderr, "pgcellar: unknown license command %q\n", args[0])
	return 2
}

func licenseKeygen(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("license keygen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("out", "", "directory that receives license_signing.key (private, 0600) and license_signing.key.pub")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" {
		_, _ = fmt.Fprintln(stderr, "pgcellar: --out DIR is required")
		return 2
	}
	if err := os.MkdirAll(*out, 0o700); err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	privPath := filepath.Join(*out, "license_signing.key")
	if _, err := os.Stat(privPath); err == nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: %s already exists; refusing to overwrite a signing key\n", privPath)
		return 1
	}
	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	if err := os.WriteFile(privPath, []byte(license.EncodeKey(priv)+"\n"), 0o600); err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	if err := os.WriteFile(privPath+".pub", []byte(license.EncodeKey(pub)+"\n"), 0o644); err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "private key: %s (keep it secret, never commit it)\npublic key:  %s.pub\n\nPaste this into internal/license/pubkey.go (PublicKeyBase64):\n%s\n", privPath, privPath, license.EncodeKey(pub))
	return 0
}

func licenseIssue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("license issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keyFile := fs.String("key", "", "private signing key written by keygen")
	edition := fs.String("edition", "", "free, pro or business")
	licensee := fs.String("licensee", "", "who the licence is for, shown in the footer")
	servers := fs.Int("servers", -1, "number of servers allowed (0 = unlimited)")
	expires := fs.String("expires", "", "last valid day, YYYY-MM-DD")
	issued := fs.String("issued", time.Now().UTC().Format("2006-01-02"), "issue day, YYYY-MM-DD")
	id := fs.String("id", "", "licence id (default: a random one)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keyFile == "" || *edition == "" || *licensee == "" || *servers < 0 || *expires == "" {
		_, _ = fmt.Fprintln(stderr, "pgcellar: --key, --edition, --licensee, --servers and --expires are required")
		return 2
	}
	priv, err := license.ReadPrivateFile(*keyFile)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: cannot read the signing key %s: %v\n", *keyFile, err)
		return 1
	}
	if *id == "" {
		b := make([]byte, 5)
		_, _ = rand.Read(b)
		*id = "lic-" + hex.EncodeToString(b)
	}
	key, err := license.Sign(priv, license.Payload{ID: *id, Edition: *edition, Licensee: *licensee, Servers: *servers, Issued: *issued, Expires: *expires})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	_, _ = fmt.Fprintln(stdout, key)
	return 0
}

func printStatus(w io.Writer, st license.Status, have int) {
	_, _ = fmt.Fprintf(w, "state:    %s\n", st.State)
	_, _ = fmt.Fprintf(w, "edition:  %s\n", st.EditionName())
	if st.Licensee != "" {
		_, _ = fmt.Fprintf(w, "licensee: %s\n", st.Licensee)
	}
	if st.ID != "" {
		_, _ = fmt.Fprintf(w, "id:       %s\n", st.ID)
	}
	if st.Unlimited() {
		_, _ = fmt.Fprintln(w, "servers:  unlimited")
	} else {
		_, _ = fmt.Fprintf(w, "servers:  %d allowed\n", st.Max())
	}
	if st.Expires != "" {
		_, _ = fmt.Fprintf(w, "expires:  %s (grace until %s)\n", st.Expires, st.GraceEnd)
	}
	if st.Message != "" {
		_, _ = fmt.Fprintf(w, "problem:  %s\n", st.Message)
	}
	if have >= 0 {
		_, _ = fmt.Fprintf(w, "footer:   %s\n", st.Footer(have))
	}
}

func licenseVerify(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(stderr, "usage: pgcellar license verify <key>")
		return 2
	}
	pub := verifyKey()
	p, err := license.Parse(args[0], pub)
	if err != nil {
		_, _ = fmt.Fprintln(stdout, "verdict: INVALID -", err)
		return 1
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	_, _ = fmt.Fprintf(stdout, "%s\n\n", b)
	st := license.Evaluate(args[0], pub, time.Now())
	printStatus(stdout, st, -1)
	_, _ = fmt.Fprintf(stdout, "verdict: signature OK, licence %s\n", st.State)
	if st.State == license.StateExpired {
		return 1
	}
	return 0
}

func licenseShow(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("license show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/pgcellar/pgcellar.yaml", "path of pgcellar.yaml")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: cannot read %s: %v\n", *cfgPath, err)
		return 1
	}
	key, err := license.KeyText(cfg.License.Key, cfg.License.File)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	st := license.Evaluate(key, verifyKey(), time.Now())
	printStatus(stdout, st, len(cfg.Servers))
	return 0
}
