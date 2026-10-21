package alerts

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

var sample = Message{
	Kind: KindLevel, Server: "pg-main", Level: LevelFail, Title: "pg-main is failing",
	Lines:  []string{"The last backup is 3 days old.", "R&D <test>"},
	Action: "Open the server page and fix the failed check.", URL: "https://cellar.example.com/servers/pg-main",
	At: time.Date(2026, 10, 20, 8, 30, 0, 0, time.UTC),
}

func TestMessageRendering(t *testing.T) {
	txt := sample.Text()
	for _, want := range []string{"❌ pg-main is failing", "- The last backup is 3 days old.", "What to do: Open the server page", "https://cellar.example.com/servers/pg-main"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text lacks %q:\n%s", want, txt)
		}
	}
	h := sample.HTML()
	for _, want := range []string{"<b>❌ pg-main is failing</b>", "R&amp;D &lt;test&gt;", `<a href="https://cellar.example.com/servers/pg-main">`} {
		if !strings.Contains(h, want) {
			t.Errorf("html lacks %q:\n%s", want, h)
		}
	}
	if Emoji(LevelOK) != "✅" || Emoji(LevelWarn) != "⚠️" {
		t.Error("emoji")
	}
}

func TestTelegram(t *testing.T) {
	var gotPath string
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	cfg := config.Alerts{Telegram: &config.TelegramAlert{TokenEnv: "TG", ChatID: "-100123"}}
	ch := Channels(cfg, Options{Getenv: env(map[string]string{"TG": "123:SECRET"}), TelegramBase: srv.URL})
	if len(ch) != 1 {
		t.Fatal("one channel expected")
	}
	if res := Send(context.Background(), ch, "", sample); len(res) != 1 || !res[0].OK {
		t.Fatalf("send: %+v", res)
	}
	if gotPath != "/bot123:SECRET/sendMessage" {
		t.Errorf("path %q", gotPath)
	}
	if got["chat_id"] != "-100123" || got["parse_mode"] != "HTML" || !strings.Contains(got["text"].(string), "<b>❌ pg-main is failing</b>") {
		t.Errorf("payload %+v", got)
	}
}

func TestTelegramErrorsNeverShowTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"ok":false,"description":"Unauthorized"}`)
	}))
	cfg := config.Alerts{Telegram: &config.TelegramAlert{TokenEnv: "TG", ChatID: "1"}}
	o := Options{Getenv: env(map[string]string{"TG": "123:SECRET"}), TelegramBase: srv.URL}
	res := Send(context.Background(), Channels(cfg, o), "", sample)
	if res[0].OK || !strings.Contains(res[0].Error, "401") || strings.Contains(res[0].Error, "SECRET") {
		t.Errorf("%+v", res[0])
	}
	srv.Close() // connection refused: Go quotes the URL, with the token in it
	res = Send(context.Background(), Channels(cfg, o), "", sample)
	if res[0].OK || strings.Contains(res[0].Error, "SECRET") {
		t.Errorf("%+v", res[0])
	}
	// variable missing
	res = Send(context.Background(), Channels(cfg, Options{Getenv: env(nil)}), "", sample)
	if res[0].OK || !strings.Contains(res[0].Error, "TG is not set") {
		t.Errorf("%+v", res[0])
	}
	ch := Channels(cfg, Options{Getenv: env(nil)})[0]
	if e := ch.Env(); len(e) != 1 || e[0].Set || e[0].Name != "TG" {
		t.Errorf("env %+v", e)
	}
}

func TestWebhookSignature(t *testing.T) {
	var body []byte
	var sig, ctype string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		sig, ctype = r.Header.Get(SignatureHeader), r.Header.Get("Content-Type")
	}))
	defer srv.Close()
	cfg := config.Alerts{Webhook: &config.WebhookAlert{URL: srv.URL, SecretEnv: "WH"}}
	ch := Channels(cfg, Options{Getenv: env(map[string]string{"WH": "s3cret"})})
	if res := Send(context.Background(), ch, "", sample); !res[0].OK {
		t.Fatalf("%+v", res)
	}
	if ctype != "application/json" || sig != Sign("s3cret", body) || !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature %q for %s", sig, body)
	}
	var p map[string]any
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatal(err)
	}
	if p["kind"] != "level" || p["server"] != "pg-main" || p["level"] != "fail" || p["title"] != "pg-main is failing" ||
		p["action"] == "" || p["url"] == "" || !strings.Contains(p["text"].(string), "❌") {
		t.Errorf("payload %v", p)
	}
	// no secret: no header
	sig = "unset"
	cfg.Webhook.SecretEnv = ""
	Send(context.Background(), Channels(cfg, Options{}), "", sample)
	if sig != "" {
		t.Errorf("unsigned webhook must not carry a signature, got %q", sig)
	}
	// a secret named but missing: refused, nothing sent
	cfg.Webhook.SecretEnv = "WH"
	if res := Send(context.Background(), Channels(cfg, Options{Getenv: env(nil)}), "", sample); res[0].OK {
		t.Error("missing secret must fail")
	}
	// HTTP error
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	cfg.Webhook = &config.WebhookAlert{URL: bad.URL}
	if res := Send(context.Background(), Channels(cfg, Options{}), "", sample); res[0].OK || !strings.Contains(res[0].Error, "500") {
		t.Errorf("%+v", res)
	}
}

// ---- a minimal SMTP server ----

type smtpMsg struct {
	From, Auth string
	To         []string
	Data       string
	TLS        bool
}

type fakeSMTP struct {
	ln   net.Listener
	mu   sync.Mutex
	msgs []smtpMsg
	cert *tls.Certificate
}

func newFakeSMTP(t *testing.T, cert *tls.Certificate) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, cert: cert}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeSMTP) port() int { return f.ln.Addr().(*net.TCPAddr).Port }

func (f *fakeSMTP) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	var cur smtpMsg
	r := bufio.NewReader(c)
	say := func(s string) { _, _ = io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"):
			if f.cert != nil && !cur.TLS {
				_, _ = io.WriteString(c, "250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN\r\n")
			} else {
				_, _ = io.WriteString(c, "250-fake\r\n250 AUTH PLAIN\r\n")
			}
		case up == "STARTTLS":
			say("220 go ahead")
			tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*f.cert}})
			if tc.Handshake() != nil {
				return
			}
			c, r, cur.TLS = tc, bufio.NewReader(tc), true
			say = func(s string) { _, _ = io.WriteString(tc, s+"\r\n") }
		case strings.HasPrefix(up, "AUTH PLAIN"):
			cur.Auth = strings.TrimSpace(line[len("AUTH PLAIN"):])
			say("235 ok")
		case strings.HasPrefix(up, "MAIL FROM:"):
			cur.From = strings.Trim(line[len("MAIL FROM:"):], "<> ")
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.To = append(cur.To, strings.Trim(line[len("RCPT TO:"):], "<> "))
			say("250 ok")
		case up == "DATA":
			say("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil || l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			cur.Data = b.String()
			f.mu.Lock()
			f.msgs = append(f.msgs, cur)
			f.mu.Unlock()
			cur = smtpMsg{TLS: cur.TLS}
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func (f *fakeSMTP) got() []smtpMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]smtpMsg(nil), f.msgs...)
}

func decodeMail(t *testing.T, raw string) (subject, body string) {
	t.Helper()
	m, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err = new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(quotedprintable.NewReader(m.Body))
	if err != nil {
		t.Fatal(err)
	}
	return subject, string(b)
}

func TestMailPlain(t *testing.T) {
	f := newFakeSMTP(t, nil)
	off := false
	cfg := config.Alerts{Mail: &config.MailAlert{Host: "127.0.0.1", Port: f.port(), StartTLS: &off, From: "pgcellar@example.com",
		To: []string{"dba@example.com", "ops@example.com"}, UserEnv: "U", PasswordEnv: "P"}}
	ch := Channels(cfg, Options{Getenv: env(map[string]string{"U": "bob", "P": "pw"})})
	if res := Send(context.Background(), ch, "", sample); !res[0].OK {
		t.Fatalf("%+v", res)
	}
	msgs := f.got()
	if len(msgs) != 1 {
		t.Fatalf("%d messages", len(msgs))
	}
	m := msgs[0]
	if m.From != "pgcellar@example.com" || len(m.To) != 2 || m.TLS || m.Auth == "" {
		t.Errorf("envelope %+v", m)
	}
	subj, body := decodeMail(t, m.Data)
	if subj != "[pgCellar] ❌ pg-main is failing" {
		t.Errorf("subject %q", subj)
	}
	if !strings.Contains(body, "The last backup is 3 days old.") || !strings.Contains(body, "What to do:") || !strings.Contains(body, sample.URL) {
		t.Errorf("body:\n%s", body)
	}
	// credentials missing
	if res := Send(context.Background(), Channels(cfg, Options{Getenv: env(nil)}), "", sample); res[0].OK || !strings.Contains(res[0].Error, "U is not set") {
		t.Errorf("%+v", res)
	}
}

func selfSigned(t *testing.T) (*tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "127.0.0.1"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func TestMailStartTLS(t *testing.T) {
	cert, pool := selfSigned(t)
	f := newFakeSMTP(t, cert)
	cfg := config.Alerts{Mail: &config.MailAlert{Host: "127.0.0.1", Port: f.port(), From: "p@example.com", To: []string{"d@example.com"}}}
	ch := Channels(cfg, Options{TLS: &tls.Config{RootCAs: pool}})
	if res := Send(context.Background(), ch, "", sample); !res[0].OK {
		t.Fatalf("%+v", res)
	}
	if m := f.got(); len(m) != 1 || !m[0].TLS {
		t.Errorf("the message must travel over TLS: %+v", m)
	}
	// a server without STARTTLS is refused unless starttls is turned off
	plain := newFakeSMTP(t, nil)
	cfg.Mail.Port = plain.port()
	if res := Send(context.Background(), Channels(cfg, Options{}), "", sample); res[0].OK || !strings.Contains(res[0].Error, "STARTTLS") {
		t.Errorf("%+v", res)
	}
}

func TestSendOnlyAndTimeout(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer srv.Close()
	cfg := config.Alerts{Webhook: &config.WebhookAlert{URL: srv.URL}, Telegram: &config.TelegramAlert{TokenEnv: "TG", ChatID: "1"}}
	ch := Channels(cfg, Options{})
	if len(ch) != 2 {
		t.Fatalf("%d channels", len(ch))
	}
	if res := Send(context.Background(), ch, Webhook, sample); len(res) != 1 || res[0].Channel != Webhook || hits != 1 {
		t.Errorf("%+v hits=%d", res, hits)
	}
	// a cancelled context ends the send at once
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := Send(ctx, ch, Webhook, sample); res[0].OK {
		t.Error("a cancelled send must fail")
	}
}
