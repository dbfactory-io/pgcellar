package barman

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBarmanHome is Barman's own default for barman_home.
const DefaultBarmanHome = "/var/lib/barman"

// XlogDBPath returns the WAL catalogue file of a server. Barman 3.20 names it
// <server>-xlog.db; older versions used xlog.db. The first one that exists is
// returned; when neither does, the 3.20 name (for the error message).
func XlogDBPath(home, server string) string {
	dir := filepath.Join(home, server, "wals")
	for _, name := range []string{server + "-xlog.db", "xlog.db"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, server+"-xlog.db")
}

// ReadLastWAL reads the last non-empty line of the server's WAL catalogue:
// "<wal name>\t<size>\t<epoch>\t<compression>\t<encryption>", one line per
// archived WAL, appended in archive order, so the last line is the last WAL
// archived. An empty file (nothing archived yet) gives an empty name and a
// zero time without error. The file is only read, never written, and only its
// last 8 KiB are looked at. path is returned even on error.
func ReadLastWAL(home, server string) (path, name string, when time.Time, err error) {
	path = XlogDBPath(home, server)
	f, err := os.Open(path)
	if err != nil {
		return path, "", time.Time{}, unwrapPathError(err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return path, "", time.Time{}, err
	}
	const tail = 8192
	if fi.Size() > tail {
		if _, err := f.Seek(-tail, io.SeekEnd); err != nil {
			return path, "", time.Time{}, err
		}
	}
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if err := sc.Err(); err != nil {
		return path, "", time.Time{}, err
	}
	if last == "" {
		return path, "", time.Time{}, nil
	}
	name, when, err = ParseXlogDBLine(last)
	return path, name, when, err
}

// ParseXlogDBLine reads one catalogue line (tab or space separated).
func ParseXlogDBLine(line string) (string, time.Time, error) {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return "", time.Time{}, fmt.Errorf("unexpected line in the WAL catalogue: %q", line)
	}
	t := parseTime(fields[2])
	if t.IsZero() {
		return "", time.Time{}, fmt.Errorf("no archive time in the WAL catalogue line: %q", line)
	}
	return fields[0], t, nil
}

func unwrapPathError(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
