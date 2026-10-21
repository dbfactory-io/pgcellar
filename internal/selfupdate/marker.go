package selfupdate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// File names, next to the executable.
const (
	markerName = "pgcellar.update.json"
	failedName = "pgcellar.update.failed.json"
	prevSuffix = ".prev"
)

// Marker is written just before the switch and read at every start: it is
// how a freshly started binary knows it is on probation.
type Marker struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	StartedAt time.Time `json:"started_at"`
	Attempts  int       `json:"attempts"`
}

// FailedVersion is a version that was installed, did not stay up and was
// rolled back. It is never tried again.
type FailedVersion struct {
	Version  string    `json:"version"`
	FailedAt time.Time `json:"failed_at"`
	Reason   string    `json:"reason"`
}

type failedFile struct {
	Versions []FailedVersion `json:"versions"`
}

func markerPath(exe string) string { return filepath.Join(filepath.Dir(exe), markerName) }
func failedPath(exe string) string { return filepath.Join(filepath.Dir(exe), failedName) }

// readMarker returns (nil, nil) when there is no marker.
func readMarker(exe string) (*Marker, error) {
	b, err := os.ReadFile(markerPath(exe))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeMarker(exe string, m Marker) error {
	return writeJSON(markerPath(exe), m)
}

func readFailed(exe string) ([]FailedVersion, error) {
	b, err := os.ReadFile(failedPath(exe))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f failedFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return f.Versions, nil
}

func addFailed(exe string, v FailedVersion) error {
	list, err := readFailed(exe)
	if err != nil {
		// A damaged file must not hide the new failure: start again.
		list = nil
	}
	if !slices.ContainsFunc(list, func(f FailedVersion) bool { return normalize(f.Version) == normalize(v.Version) }) {
		list = append(list, v)
	}
	return writeJSON(failedPath(exe), failedFile{Versions: list})
}

// writeJSON writes through a temporary file and a rename, so a crash never
// leaves half a file.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
