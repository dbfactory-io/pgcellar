package vault

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Exit codes of restic that mean something to pgCellar.
const (
	ExitPartial   = 3  // backup: some files could not be read, the snapshot exists
	ExitNoRepo    = 10 // the repository does not exist (restic 0.17 and later)
	ExitLocked    = 11 // the repository is locked by another process
	ExitWrongPass = 12 // the password does not open the repository
)

// Host is the host name pgCellar gives every snapshot: the container
// hostname changes at each recreation, and restic uses the host to find the
// previous snapshot of the same paths.
const Host = "pgcellar"

// Repo is the restic repository of a server:
// s3:<endpoint>/<bucket>/<prefix>/<server>.
func Repo(endpoint, bucket, prefix, server string) string {
	p := strings.Trim(prefix, "/")
	r := "s3:" + strings.TrimRight(endpoint, "/") + "/" + bucket
	if p != "" {
		r += "/" + p
	}
	return r + "/" + server
}

// ServerPrefix is the folder of a server in the bucket, with a trailing
// slash ("pgcellar/pg-main/").
func ServerPrefix(prefix, server string) string {
	p := strings.Trim(prefix, "/")
	if p == "" {
		return server + "/"
	}
	return p + "/" + server + "/"
}

// NewPassword returns a random restic password: 32 bytes, base64url.
func NewPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BackupSummary is the last line of `restic backup --json`.
type BackupSummary struct {
	SnapshotID          string  `json:"snapshot_id"`
	FilesNew            int64   `json:"files_new"`
	FilesChanged        int64   `json:"files_changed"`
	FilesUnmodified     int64   `json:"files_unmodified"`
	DataAdded           int64   `json:"data_added"`
	DataAddedPacked     int64   `json:"data_added_packed"`
	TotalFilesProcessed int64   `json:"total_files_processed"`
	TotalBytesProcessed int64   `json:"total_bytes_processed"`
	TotalDuration       float64 `json:"total_duration"`
}

// Uploaded is what reached the bucket: the packed size when restic says it.
func (s BackupSummary) Uploaded() int64 {
	if s.DataAddedPacked > 0 {
		return s.DataAddedPacked
	}
	return s.DataAdded
}

// ParseBackup reads the summary of `restic backup --json`. It also returns
// the error messages restic printed for files it could not read.
func ParseBackup(out []byte) (BackupSummary, []string, error) {
	var sum BackupSummary
	var found bool
	var warnings []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var head struct {
			Type  string `json:"message_type"`
			Item  string `json:"item"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.Type {
		case "summary":
			if err := json.Unmarshal(line, &sum); err != nil {
				return sum, warnings, err
			}
			found = true
		case "error":
			w := strings.TrimSpace(head.Item + ": " + head.Error.Message)
			if len(warnings) < 20 {
				warnings = append(warnings, strings.TrimPrefix(w, ": "))
			}
		}
	}
	if !found {
		return sum, warnings, errors.New("restic printed no summary")
	}
	return sum, warnings, nil
}

// Snapshot is one entry of `restic snapshots --json`.
type Snapshot struct {
	ID      string    `json:"id"`
	ShortID string    `json:"short_id"`
	Time    time.Time `json:"time"`
	Paths   []string  `json:"paths"`
	Tags    []string  `json:"tags"`
	Host    string    `json:"hostname"`
}

// ParseSnapshots reads `restic snapshots --json`, oldest first.
func ParseSnapshots(out []byte) ([]Snapshot, error) {
	var list []Snapshot
	if err := json.Unmarshal(bytes.TrimSpace(out), &list); err != nil {
		return nil, fmt.Errorf("the snapshot list of restic is not readable: %w", err)
	}
	return list, nil
}

// Stats is `restic stats --json --mode raw-data`.
type Stats struct {
	TotalSize             int64 `json:"total_size"`
	TotalUncompressedSize int64 `json:"total_uncompressed_size"`
	SnapshotsCount        int   `json:"snapshots_count"`
}

// ParseStats reads `restic stats --json`.
func ParseStats(out []byte) (Stats, error) {
	var s Stats
	if err := json.Unmarshal(bytes.TrimSpace(out), &s); err != nil {
		return s, fmt.Errorf("the statistics of restic are not readable: %w", err)
	}
	return s, nil
}

// Explain turns what restic printed on its standard error into one sentence
// for people, or "" when nothing is recognised.
func Explain(code int, stderr string) string {
	s := strings.ToLower(stderr)
	switch {
	case code == ExitWrongPass || strings.Contains(s, "wrong password"):
		return "the restic password does not open this repository: the password file is not the one the copy was made with"
	case code == ExitLocked || strings.Contains(s, "repository is already locked"):
		return "the repository is locked by another restic process"
	case code == ExitNoRepo || strings.Contains(s, "is there a repository at the following location"):
		return "the repository does not exist yet"
	case strings.Contains(s, "access denied") || strings.Contains(s, "accessdenied"):
		return "the S3 service refused the access key: check the access key, the secret key and the rights of the user on the bucket"
	case strings.Contains(s, "nosuchbucket") || strings.Contains(s, "bucket does not exist"):
		return "the bucket does not exist: create it first"
	case strings.Contains(s, "no such host") || strings.Contains(s, "connection refused") || strings.Contains(s, "i/o timeout") || strings.Contains(s, "network is unreachable"):
		return "the S3 endpoint cannot be reached from this host"
	case strings.Contains(s, "no space left"):
		return "the disk of this host is full"
	}
	return ""
}
