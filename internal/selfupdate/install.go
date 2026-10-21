package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Limits of a download and of the binary inside it.
const (
	maxDownload    = 200 << 20
	versionTimeout = 10 * time.Second
)

// errExec marks a failed exec after a successful switch (the old binary was
// put back).
var errExec = errors.New("exec of the new binary failed")

// install downloads, checks and switches to rel's binary for this platform,
// then execs it. It returns only when something failed; everything it wrote
// is removed and the running binary is left as it was.
func (u *Updater) install(ctx context.Context, exe string, rel Release, bin Binary) error {
	dir := filepath.Dir(exe)
	archive, err := u.download(ctx, dir, bin)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(archive) }()

	fresh, err := extract(archive, dir)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.Remove(fresh)
		}
	}()

	vctx, cancel := context.WithTimeout(ctx, versionTimeout)
	out, err := u.Sys.RunVersion(vctx, fresh)
	cancel()
	if err != nil {
		return fmt.Errorf("the new binary does not run: %v", err)
	}
	if !mentions(out, rel.Version) {
		return fmt.Errorf("the new binary says %q, expected version %s", strings.TrimSpace(firstLine(out)), normalize(rel.Version))
	}

	keep = true
	return u.switchTo(exe, fresh, rel.Version)
}

// download fetches bin.URL (HTTPS only, at most 200 MB) into a temporary file
// of dir and checks its SHA-256 against the manifest.
func (u *Updater) download(ctx context.Context, dir string, bin Binary) (string, error) {
	pu, err := url.Parse(bin.URL)
	if err != nil || pu.Scheme != "https" || pu.Host == "" {
		return "", fmt.Errorf("refusing to download %q: only https URLs are accepted", bin.URL)
	}
	want := strings.ToLower(strings.TrimSpace(bin.SHA256))
	if len(want) != 64 {
		return "", errors.New("the manifest gives no valid SHA-256 for this binary")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, bin.URL, nil)
	if err != nil {
		return "", err
	}
	client := u.Sys.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download: %s answered %s", pu.Host, resp.Status)
	}
	if resp.ContentLength > maxDownload {
		return "", fmt.Errorf("download: %d bytes is more than the %d allowed", resp.ContentLength, maxDownload)
	}

	f, err := os.CreateTemp(dir, ".pgcellar-download-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(name)
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return "", fmt.Errorf("download: %v", err)
	}
	if n > maxDownload {
		return "", fmt.Errorf("download: more than the %d bytes allowed", maxDownload)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("checksum mismatch: the archive has SHA-256 %s, the manifest says %s", got, want)
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

// extract takes the `pgcellar` entry out of a tar.gz into a temporary file of
// dir, mode 0755. Any entry with an absolute path or a ".." element makes the
// whole archive refused, whether or not it is the binary.
func extract(archive, dir string) (string, error) {
	in, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	defer func() { _ = in.Close() }()
	zr, err := gzip.NewReader(in)
	if err != nil {
		return "", fmt.Errorf("archive: %v", err)
	}
	tr := tar.NewReader(zr)

	var out string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if out != "" {
				_ = os.Remove(out)
			}
			return "", fmt.Errorf("archive: %v", err)
		}
		if unsafePath(hdr.Name) || (hdr.Linkname != "" && unsafePath(hdr.Linkname)) {
			if out != "" {
				_ = os.Remove(out)
			}
			return "", fmt.Errorf("archive: unsafe path %q, refused", hdr.Name)
		}
		if path.Clean(hdr.Name) != "pgcellar" || out != "" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return "", errors.New("archive: the pgcellar entry is not a regular file")
		}
		out, err = writeEntry(tr, dir, hdr.Size)
		if err != nil {
			return "", err
		}
	}
	if out == "" {
		return "", errors.New("archive: no pgcellar binary inside")
	}
	return out, nil
}

func writeEntry(r io.Reader, dir string, size int64) (string, error) {
	if size > maxDownload {
		return "", errors.New("archive: the pgcellar entry is too large")
	}
	f, err := os.CreateTemp(dir, ".pgcellar-new-*")
	if err != nil {
		return "", err
	}
	name := f.Name()
	_, err = io.Copy(f, io.LimitReader(r, maxDownload))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, 0o755)
	}
	if err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("archive: %v", err)
	}
	return name, nil
}

func unsafePath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") || filepath.IsAbs(p) {
		return true
	}
	for _, e := range strings.Split(strings.ReplaceAll(p, "\\", "/"), "/") {
		if e == ".." {
			return true
		}
	}
	return false
}

// switchTo writes the marker, moves the running binary to .prev, puts the new
// one in its place and execs it. When exec fails the old binary is put back
// and the marker removed.
func (u *Updater) switchTo(exe, fresh, to string) error {
	m := Marker{From: normalize(u.Current), To: normalize(to), StartedAt: u.Sys.Now().UTC(), Attempts: 0}
	if err := writeMarker(exe, m); err != nil {
		return fmt.Errorf("cannot write the update marker: %v", err)
	}
	prev := exe + prevSuffix
	if err := os.Rename(exe, prev); err != nil {
		_ = os.Remove(markerPath(exe))
		return fmt.Errorf("cannot move the current binary aside: %v", err)
	}
	if err := os.Rename(fresh, exe); err != nil {
		rerr := os.Rename(prev, exe)
		_ = os.Remove(markerPath(exe))
		if rerr != nil {
			return fmt.Errorf("cannot put the new binary in place (%v) nor the old one back (%v)", err, rerr)
		}
		return fmt.Errorf("cannot put the new binary in place: %v", err)
	}
	u.logf("selfupdate: switching from %s to %s, exec %s", m.From, m.To, exe)
	err := u.Sys.Exec(exe, u.Sys.Args(), u.Sys.Environ())
	// Only reached when exec failed.
	if rerr := os.Rename(prev, exe); rerr != nil {
		u.logf("selfupdate: exec failed and the old binary could not be put back: %v", rerr)
		return fmt.Errorf("%w: %v; the old binary is still at %s", errExec, err, prev)
	}
	_ = os.Remove(markerPath(exe))
	return fmt.Errorf("%w: %v; the old binary was put back", errExec, err)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
