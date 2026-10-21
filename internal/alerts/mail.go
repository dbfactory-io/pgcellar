package alerts

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

type mailer struct {
	cfg config.MailAlert
	o   Options
}

func (m *mailer) Name() string { return Mail }

func (m *mailer) Target() string { return strings.Join(m.cfg.To, ", ") }

func (m *mailer) Env() []EnvStatus { return envStatus(m.o, m.cfg.UserEnv, m.cfg.PasswordEnv) }

func (m *mailer) port() int {
	if m.cfg.Port > 0 {
		return m.cfg.Port
	}
	if m.startTLS() {
		return 587
	}
	return 25
}

func (m *mailer) startTLS() bool { return m.cfg.UsesStartTLS() }

// plainAuth is AUTH PLAIN without net/smtp's refusal to run unencrypted:
// the administrator turned starttls off on purpose.
type plainAuth struct{ user, pass string }

func (a plainAuth) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.user + "\x00" + a.pass), nil
}

func (a plainAuth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, errors.New("unexpected server challenge")
	}
	return nil, nil
}

func (m *mailer) Send(ctx context.Context, msg Message) error {
	if len(m.cfg.To) == 0 || m.cfg.From == "" {
		return errors.New("alerts.mail needs from and to")
	}
	var user, pass string
	if m.cfg.UserEnv != "" {
		user = m.o.getenv(m.cfg.UserEnv)
		if user == "" {
			return fmt.Errorf("the environment variable %s is not set", m.cfg.UserEnv)
		}
		var err error
		if pass, err = needEnv(m.o, m.cfg.PasswordEnv); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(Timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	addr := net.JoinHostPort(m.cfg.Host, strconv.Itoa(m.port()))
	tlsCfg := &tls.Config{ServerName: m.cfg.Host, MinVersion: tls.VersionTLS12}
	if m.o.TLS != nil {
		tlsCfg = m.o.TLS.Clone()
		if tlsCfg.ServerName == "" {
			tlsCfg.ServerName = m.cfg.Host
		}
	}
	d := net.Dialer{Deadline: deadline}
	var conn net.Conn
	var err error
	if m.port() == 465 {
		conn, err = (&tls.Dialer{NetDialer: &d, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, m.cfg.Host)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	if m.port() != 465 && m.startTLS() {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the mail server does not offer STARTTLS; set starttls: false only on a network you trust")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if user != "" {
		if err := c.Auth(plainAuth{user, pass}); err != nil {
			return err
		}
	}
	if err := c.Mail(m.cfg.From); err != nil {
		return err
	}
	for _, to := range m.cfg.To {
		if err := c.Rcpt(to); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(m.render(msg)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// render builds the RFC 5322 message: UTF-8 text, quoted-printable.
func (m *mailer) render(msg Message) []byte {
	var id [8]byte
	_, _ = rand.Read(id[:])
	host := m.cfg.Host
	if i := strings.LastIndex(m.cfg.From, "@"); i >= 0 {
		host = strings.Trim(m.cfg.From[i+1:], "> ")
	}
	var b bytes.Buffer
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", m.cfg.From)
	h("To", strings.Join(m.cfg.To, ", "))
	h("Subject", mime.QEncoding.Encode("utf-8", "[pgCellar] "+msg.Subject()))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id[:])+"@"+host+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=utf-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&b)
	_, _ = qp.Write([]byte(strings.ReplaceAll(msg.Text(), "\n", "\r\n") + "\r\n"))
	_ = qp.Close()
	return b.Bytes()
}
