package config

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Updates configures the check for new pgCellar releases. Every key is a
// pointer: nil = absent = the default, so Save never writes a default.
//
// pgCellar only reads the release list (one GET on Source, then one on the
// manifest of the release); nothing is sent but the running version, in the
// User-Agent header. Installing a release is not done here.
type Updates struct {
	// Check: look for a new release every Interval. Absent = true.
	Check *bool `yaml:"check,omitempty" json:"check"`
	// Interval between two checks. Absent = 24h, at least 1h.
	Interval *time.Duration `yaml:"interval,omitempty" json:"interval"`
	// Channel: only "stable" exists for now. Absent = stable.
	Channel *string `yaml:"channel,omitempty" json:"channel"`
	// Auto: install new releases by itself, inside Window. Absent = false.
	// Read here, applied by the self-update code (or ignored in Docker, where
	// the host updater does the work).
	Auto *bool `yaml:"auto,omitempty" json:"auto"`
	// Window: when an update may be applied, "HH:MM-HH:MM" in the local time
	// of the server; it may cross midnight ("23:00-01:00"). Absent = 02:00-05:00.
	Window *string `yaml:"window,omitempty" json:"window"`
	// Source: the URL of the latest release in the GitHub API format.
	// Absent = DefaultUpdatesSource.
	Source *string `yaml:"source,omitempty" json:"source"`
}

// Defaults of the updates block.
const (
	DefaultUpdatesInterval = 24 * time.Hour
	MinUpdatesInterval     = time.Hour
	DefaultUpdatesChannel  = "stable"
	DefaultUpdatesWindow   = "02:00-05:00"
	DefaultUpdatesSource   = "https://api.github.com/repos/dbfactory-io/pgcellar/releases/latest"
)

// IsCheckEnabled resolves updates.check (absent = true).
func (u Updates) IsCheckEnabled() bool { return u.Check == nil || *u.Check }

// IsAuto resolves updates.auto (absent = false).
func (u Updates) IsAuto() bool { return u.Auto != nil && *u.Auto }

// EffectiveInterval resolves updates.interval (absent = 24h).
func (u Updates) EffectiveInterval() time.Duration {
	if u.Interval == nil {
		return DefaultUpdatesInterval
	}
	return *u.Interval
}

// EffectiveChannel resolves updates.channel (absent = stable).
func (u Updates) EffectiveChannel() string {
	if u.Channel == nil || strings.TrimSpace(*u.Channel) == "" {
		return DefaultUpdatesChannel
	}
	return strings.TrimSpace(*u.Channel)
}

// EffectiveWindow resolves updates.window (absent = 02:00-05:00) as text.
func (u Updates) EffectiveWindow() string {
	if u.Window == nil || strings.TrimSpace(*u.Window) == "" {
		return DefaultUpdatesWindow
	}
	return strings.TrimSpace(*u.Window)
}

// ParsedWindow is EffectiveWindow parsed; a configuration that passed
// validation always parses.
func (u Updates) ParsedWindow() Window {
	w, err := ParseWindow(u.EffectiveWindow())
	if err != nil {
		w, _ = ParseWindow(DefaultUpdatesWindow)
	}
	return w
}

// EffectiveSource resolves updates.source.
func (u Updates) EffectiveSource() string {
	if u.Source == nil || strings.TrimSpace(*u.Source) == "" {
		return DefaultUpdatesSource
	}
	return strings.TrimSpace(*u.Source)
}

// MarshalJSON shows resolved values, never a pointer or a null.
func (u Updates) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Check    bool          `json:"check"`
		Interval time.Duration `json:"interval"`
		Channel  string        `json:"channel"`
		Auto     bool          `json:"auto"`
		Window   string        `json:"window"`
		Source   string        `json:"source"`
	}{u.IsCheckEnabled(), u.EffectiveInterval(), u.EffectiveChannel(), u.IsAuto(), u.EffectiveWindow(), u.EffectiveSource()})
}

// Window is a daily time range in minutes after midnight, local time. When
// End is before Start the range crosses midnight.
type Window struct {
	Start, End int
}

// ParseWindow reads "HH:MM-HH:MM". Start and end must differ.
func ParseWindow(s string) (Window, error) {
	a, b, ok := strings.Cut(strings.TrimSpace(s), "-")
	if !ok {
		return Window{}, fmt.Errorf("%q is not a time range (use \"02:00-05:00\")", s)
	}
	h1, m1, ok1 := ParseClock(a)
	h2, m2, ok2 := ParseClock(b)
	if !ok1 || !ok2 {
		return Window{}, fmt.Errorf("%q is not a time range (use \"02:00-05:00\")", s)
	}
	w := Window{Start: h1*60 + m1, End: h2*60 + m2}
	if w.Start == w.End {
		return Window{}, fmt.Errorf("%q starts and ends at the same time", s)
	}
	return w, nil
}

// Contains reports whether t (in its own location) falls inside the window.
// The start is included, the end is not.
func (w Window) Contains(t time.Time) bool {
	m := t.Hour()*60 + t.Minute()
	if w.Start < w.End {
		return m >= w.Start && m < w.End
	}
	return m >= w.Start || m < w.End // crosses midnight
}

// String writes the window back as "HH:MM-HH:MM".
func (w Window) String() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.Start/60, w.Start%60, w.End/60, w.End%60)
}

// validateUpdates returns one sentence per problem of the updates block.
func validateUpdates(u Updates) []string {
	var p []string
	if u.Interval != nil && *u.Interval < MinUpdatesInterval {
		p = append(p, fmt.Sprintf("updates.interval must be at least %s (for example \"24h\"); leave it out for the default", MinUpdatesInterval))
	}
	if c := u.EffectiveChannel(); c != DefaultUpdatesChannel {
		p = append(p, fmt.Sprintf("updates.channel %q is not known: only \"stable\" exists for now", c))
	}
	if _, err := ParseWindow(u.EffectiveWindow()); err != nil {
		p = append(p, "updates.window: "+err.Error())
	}
	src := u.EffectiveSource()
	if pu, err := url.Parse(src); err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
		p = append(p, fmt.Sprintf("updates.source %q must be an http:// or https:// URL", src))
	}
	return p
}
