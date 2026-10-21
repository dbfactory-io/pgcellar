// Package alerts sends pgCellar's messages to the outside world: Telegram,
// mail and a signed webhook. It knows nothing about servers or Barman: the
// application decides what is worth a message (internal/app/alerts.go) and
// hands over a Message.
//
// Secrets are never read from pgcellar.yaml, only from the environment
// variables the file names. A send takes at most 10 seconds and its error is
// returned to the caller, which logs it and carries on.
package alerts

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

// Timeout bounds one send to one channel.
const Timeout = 10 * time.Second

// Channel names, as they appear in the journal and the metrics.
const (
	Telegram = "telegram"
	Mail     = "mail"
	Webhook  = "webhook"
)

// Kinds of message.
const (
	KindLevel    = "level"         // a server changed level
	KindResolved = "resolved"      // a server is healthy again
	KindReminder = "reminder"      // a problem that lasts
	KindBackup   = "backup_failed" // a backup run ended with an error
	KindDrill    = "drill_failed"  // a restore drill failed
	KindDrillOK  = "drill_ok"      // a restore drill passed
	KindSkipped  = "skipped"       // a scheduled run could not start
	KindSummary  = "summary"       // the daily summary
	KindTest     = "test"          // the "send a test message" button
)

// Levels, the same words as the server states.
const (
	LevelOK   = "ok"
	LevelWarn = "warn"
	LevelFail = "fail"
)

// Message is one thing to tell. Every channel renders it its own way.
type Message struct {
	Kind   string    `json:"kind"`
	Server string    `json:"server,omitempty"`
	Level  string    `json:"level"`
	Title  string    `json:"title"`
	Lines  []string  `json:"lines,omitempty"`  // what is wrong, one sentence each
	Action string    `json:"action,omitempty"` // what to do, when it is red or orange
	URL    string    `json:"url,omitempty"`    // where to look in pgCellar
	At     time.Time `json:"at"`
}

// Emoji of a level, the same as the interface.
func Emoji(level string) string {
	switch level {
	case LevelFail:
		return "❌"
	case LevelWarn:
		return "⚠️"
	}
	return "✅"
}

// Subject is the title with its emoji.
func (m Message) Subject() string {
	if m.Kind == KindSummary {
		return m.Title // the headline already carries its emoji
	}
	return Emoji(m.Level) + " " + m.Title
}

// Text is the plain-text body, used for mail and as the "text" of the webhook.
func (m Message) Text() string {
	var b strings.Builder
	b.WriteString(m.Subject())
	b.WriteString("\n")
	for _, l := range m.Lines {
		b.WriteString("\n- " + l)
	}
	if m.Action != "" {
		b.WriteString("\n\nWhat to do: " + m.Action)
	}
	if m.URL != "" {
		b.WriteString("\n\n" + m.URL)
	}
	return b.String()
}

// HTML is the body for Telegram (its small HTML subset: b, i, a, code).
func (m Message) HTML() string {
	var b strings.Builder
	b.WriteString("<b>" + html.EscapeString(m.Subject()) + "</b>\n")
	for _, l := range m.Lines {
		b.WriteString("\n• " + html.EscapeString(l))
	}
	if m.Action != "" {
		b.WriteString("\n\n<b>What to do:</b> " + html.EscapeString(m.Action))
	}
	if m.URL != "" {
		b.WriteString("\n\n<a href=\"" + html.EscapeString(m.URL) + "\">Open in pgCellar</a>")
	}
	return b.String()
}

// EnvStatus says whether an environment variable named in the configuration
// has a value. The value itself is never exposed.
type EnvStatus struct {
	Name string `json:"name"`
	Set  bool   `json:"set"`
}

// Channel is one way out.
type Channel interface {
	Name() string
	Send(ctx context.Context, m Message) error
	// Env lists the environment variables the channel needs and whether each is set.
	Env() []EnvStatus
	// Target is a short description without secrets ("chat -100123", "dba@example.com").
	Target() string
}

// Options let tests aim the channels at a local server.
type Options struct {
	Getenv       func(string) string // default os.Getenv
	HTTPClient   *http.Client
	TelegramBase string      // default https://api.telegram.org
	TLS          *tls.Config // extra TLS settings for the mail channel (tests)
}

func (o Options) getenv(k string) string {
	if o.Getenv != nil {
		return o.Getenv(k)
	}
	return os.Getenv(k)
}

func (o Options) client() *http.Client {
	if o.HTTPClient != nil {
		return o.HTTPClient
	}
	return &http.Client{Timeout: Timeout}
}

// Channels builds the channels the configuration enables.
func Channels(cfg config.Alerts, o Options) []Channel {
	var out []Channel
	if t := cfg.Telegram; t != nil && t.TokenEnv != "" {
		out = append(out, &telegram{cfg: *t, o: o})
	}
	if m := cfg.Mail; m != nil && m.Host != "" {
		out = append(out, &mailer{cfg: *m, o: o})
	}
	if w := cfg.Webhook; w != nil && w.URL != "" {
		out = append(out, &webhook{cfg: *w, o: o})
	}
	return out
}

// Result is the outcome of one send.
type Result struct {
	Channel string `json:"channel"`
	OK      bool   `json:"ok"`
	Error   string `json:"error,omitempty"`
}

// Send hands a message to every channel (or only to `only` when it is not
// empty), one after the other, each with its own timeout. Errors are
// returned, never fatal.
func Send(ctx context.Context, chans []Channel, only string, m Message) []Result {
	var res []Result
	for _, c := range chans {
		if only != "" && c.Name() != only {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, Timeout)
		err := c.Send(cctx, m)
		cancel()
		r := Result{Channel: c.Name(), OK: err == nil}
		if err != nil {
			r.Error = err.Error()
		}
		res = append(res, r)
	}
	return res
}

func needEnv(o Options, name string) (string, error) {
	v := o.getenv(name)
	if v == "" {
		return "", fmt.Errorf("the environment variable %s is not set", name)
	}
	return v, nil
}

func envStatus(o Options, names ...string) []EnvStatus {
	var out []EnvStatus
	for _, n := range names {
		if n != "" {
			out = append(out, EnvStatus{Name: n, Set: o.getenv(n) != ""})
		}
	}
	return out
}

// scrub removes a secret from an error text (Go's HTTP errors quote the URL,
// and the Telegram token is part of it).
func scrub(s string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			s = strings.ReplaceAll(s, x, "***")
		}
	}
	return s
}
