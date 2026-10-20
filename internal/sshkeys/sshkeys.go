// Package sshkeys owns the SSH key pair that pgCellar (the barman user) uses
// to reach PostgreSQL servers: rsync backups, and the SSH test of the WAL
// shipping setup. The private key is written once to ~/.ssh with mode 0600 and
// is never returned by any function of this package, logged, or sent to a
// page or to the API. Only the public key leaves.
package sshkeys

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"

	"github.com/stranix79/pgcellar/internal/barman"
)

const (
	privName = "id_ed25519"
	pubName  = "id_ed25519.pub"
)

// Dir returns the .ssh directory of a home directory.
func Dir(home string) string { return filepath.Join(home, ".ssh") }

// Ensure creates ~/.ssh (0700) and an ed25519 key pair (0600 / 0644) when
// there is no private key yet. It never replaces an existing key. When the
// private key exists but the public file is missing, the public file is
// written again from the private key. created reports whether a new pair was
// generated.
func Ensure(home string) (created bool, err error) {
	if home == "" {
		return false, errors.New("no home directory for the SSH key")
	}
	dir := Dir(home)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, err
	}
	priv := filepath.Join(dir, privName)
	pub := filepath.Join(dir, pubName)

	if data, err := os.ReadFile(priv); err == nil {
		if _, serr := os.Stat(pub); serr == nil {
			return false, nil
		}
		key, perr := ssh.ParseRawPrivateKey(data)
		if perr != nil {
			return false, fmt.Errorf("%s exists but cannot be read: %w", priv, perr)
		}
		signer, perr := ssh.NewSignerFromKey(key)
		if perr != nil {
			return false, perr
		}
		return false, os.WriteFile(pub, ssh.MarshalAuthorizedKey(signer.PublicKey()), 0o644)
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}

	pk, sk, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return false, err
	}
	block, err := ssh.MarshalPrivateKey(sk, "pgcellar")
	if err != nil {
		return false, err
	}
	sshPub, err := ssh.NewPublicKey(pk)
	if err != nil {
		return false, err
	}
	f, err := os.OpenFile(priv, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false, err
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		_ = f.Close()
		_ = os.Remove(priv)
		return false, err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(priv)
		return false, err
	}
	if err := os.WriteFile(pub, ssh.MarshalAuthorizedKey(sshPub), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// PublicKey returns the authorized_keys line of the public key, with the
// comment pgcellar@<hostname>. The key must exist (see Ensure).
func PublicKey(home string) (string, error) {
	data, err := os.ReadFile(filepath.Join(Dir(home), pubName))
	if err != nil {
		return "", err
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(data)
	if err != nil {
		return "", fmt.Errorf("%s is not a valid public key: %w", pubName, err)
	}
	host, _ := os.Hostname()
	if host == "" {
		host = "localhost"
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))) + " pgcellar@" + host, nil
}

// TestResult is the outcome of a connection test.
type TestResult struct {
	OK      bool
	Message string // "OK" or the error as ssh reported it
}

// TestConnection runs `<ssh_command> -o BatchMode=yes -o ConnectTimeout=10
// true` through the audited runner. The options are placed right after the
// ssh program so they cannot be taken for arguments of the remote command.
// ssh_command is split on spaces (no shell, no quoting) and must start with ssh.
func TestConnection(ctx context.Context, r barman.BinaryRunner, sshCommand string) TestResult {
	f := strings.Fields(sshCommand)
	if len(f) < 2 || filepath.Base(f[0]) != "ssh" {
		return TestResult{Message: "ssh_command must look like \"ssh postgres@db1\"."}
	}
	args := append([]string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}, f[1:]...)
	args = append(args, "true")
	_, stderr, err := r.RunBinary(ctx, f[0], args...)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = err.Error()
		}
		return TestResult{Message: msg}
	}
	return TestResult{OK: true, Message: "OK"}
}
