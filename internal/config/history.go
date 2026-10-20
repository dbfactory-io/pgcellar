package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// HistoryEntry is one commit of the configuration directory.
type HistoryEntry struct {
	Hash    string // short hash
	Date    string // RFC3339
	Author  string
	Message string
}

// IgnoredPatterns are never committed: secrets stay out of the history.
var IgnoredPatterns = []string{"*.key", "id_*", "*.pem", "*.db"}

func git(dir string, extraEnv []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, extraEnv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// EnsureRepo initialises a git repository in dir if there is none and writes
// a .gitignore excluding secrets.
func EnsureRepo(dir string) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if _, err := git(dir, nil, "init", "-q"); err != nil {
			return err
		}
	}
	ignore := filepath.Join(dir, ".gitignore")
	existing, _ := os.ReadFile(ignore)
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var add []string
	for _, p := range IgnoredPatterns {
		if !have[p] {
			add = append(add, p)
		}
	}
	if len(add) == 0 {
		return nil
	}
	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += strings.Join(add, "\n") + "\n"
	return os.WriteFile(ignore, []byte(content), 0o644)
}

// ImportExisting makes the first commit of a repository that has none, so the
// configuration found on disk is the baseline of the history. It does nothing
// when a commit already exists. It returns the new short hash, or "".
func ImportExisting(dir string) (string, error) {
	if err := EnsureRepo(dir); err != nil {
		return "", err
	}
	if _, err := git(dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err == nil {
		return "", nil
	}
	return Commit(dir, "Import the existing configuration", "pgcellar", "pgcellar@localhost")
}

var safeIdent = regexp.MustCompile(`[<>\n\r]`)

// Commit records all changes in dir. It returns the new short hash, or ""
// when there was nothing to commit.
func Commit(dir, message, authorName, authorEmail string) (string, error) {
	if err := EnsureRepo(dir); err != nil {
		return "", err
	}
	if _, err := git(dir, nil, "add", "-A"); err != nil {
		return "", err
	}
	status, err := git(dir, nil, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) == "" {
		return "", nil
	}
	if authorName == "" {
		authorName = "pgcellar"
	}
	if authorEmail == "" {
		authorEmail = "pgcellar@localhost"
	}
	authorName = safeIdent.ReplaceAllString(authorName, "")
	authorEmail = safeIdent.ReplaceAllString(authorEmail, "")
	env := []string{
		"GIT_AUTHOR_NAME=" + authorName, "GIT_AUTHOR_EMAIL=" + authorEmail,
		"GIT_COMMITTER_NAME=" + authorName, "GIT_COMMITTER_EMAIL=" + authorEmail,
	}
	if message == "" {
		message = "update configuration"
	}
	if _, err := git(dir, env, "-c", "commit.gpgsign=false", "commit", "-q", "-m", message); err != nil {
		return "", err
	}
	out, err := git(dir, nil, "rev-parse", "--short", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Log returns the n most recent commits, newest first. An empty repository
// yields an empty list.
func Log(dir string, n int) ([]HistoryEntry, error) {
	if n <= 0 {
		n = 50
	}
	if _, err := git(dir, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return nil, nil
	}
	out, err := git(dir, nil, "log", fmt.Sprintf("-n%d", n), "--date=iso-strict", "--format=%h%x1f%aI%x1f%an%x1f%s")
	if err != nil {
		return nil, err
	}
	var res []HistoryEntry
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "\x1f", 4)
		if len(f) != 4 {
			continue
		}
		res = append(res, HistoryEntry{Hash: f[0], Date: f[1], Author: f[2], Message: f[3]})
	}
	return res, nil
}

var hashRe = regexp.MustCompile(`^[0-9a-f]{4,40}$`)

// Diff returns the patch introduced by one commit.
func Diff(dir, hash string) (string, error) {
	if !hashRe.MatchString(hash) {
		return "", fmt.Errorf("invalid commit hash %q", hash)
	}
	return git(dir, nil, "show", "--format=%h %an %aI%n%s%n", "--patch", hash)
}
