package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/store"
)

const (
	cookieName  = "pgcellar_session"
	sessionTTL  = 24 * time.Hour
	loginTokTTL = time.Hour
)

// userCtx is the signed-in user of a request.
type userCtx struct {
	Name, Role, CSRF string
	sessionID        string
}

// dummyHash makes a login for an unknown user cost as much as a real one.
var dummyHash, _ = config.HashPassword("pgcellar-not-a-password")

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func (s *Server) mac(parts ...string) string {
	m := hmac.New(sha256.New, s.secret)
	for _, p := range parts {
		m.Write([]byte(p))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

func (s *Server) csrfFor(sessionID string) string { return s.mac("csrf", sessionID) }

// loginToken is a stateless CSRF token for the login form, which has no
// session yet: "<unix time>.<mac>", valid for one hour.
func (s *Server) loginToken() string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	return ts + "." + s.mac("login", ts)
}

func (s *Server) loginTokenOK(tok string) bool {
	ts, sig, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(n, 0)) > loginTokTTL || time.Until(time.Unix(n, 0)) > time.Minute {
		return false
	}
	return hmac.Equal([]byte(sig), []byte(s.mac("login", ts)))
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// currentUser resolves the session cookie. The role is read from the
// configuration at every request, so removing a user ends their access.
func (s *Server) currentUser(r *http.Request) *userCtx {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	id := hashToken(c.Value)
	sess, err := s.app.Store.GetSession(id)
	if err != nil {
		return nil
	}
	for _, u := range s.app.Config().Users {
		if u.Name == sess.User {
			return &userCtx{Name: u.Name, Role: u.Role, CSRF: s.csrfFor(id), sessionID: id}
		}
	}
	return nil
}

type guarded func(w http.ResponseWriter, r *http.Request, u *userCtx)

// guard requires a signed-in user with at least the given role, and a valid
// CSRF token on every POST.
func (s *Server) guard(min string, h guarded) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			if isHX(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		if !app.RoleAtLeast(u.Role, min) {
			s.errorPage(w, r, u, http.StatusForbidden, "Your role ("+u.Role+") does not allow this. Ask an administrator.")
			return
		}
		if r.Method == http.MethodPost {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				_ = r.ParseForm()
				tok = r.PostForm.Get("csrf")
			}
			if !hmac.Equal([]byte(tok), []byte(u.CSRF)) {
				s.errorPage(w, r, u, http.StatusForbidden, "The security token of this form is missing or expired. Reload the page and try again.")
				return
			}
		}
		h(w, r, u)
	}
}

type loginData struct{ Token, Next string }

func safeNext(n string) string {
	if strings.HasPrefix(n, "/") && !strings.HasPrefix(n, "//") && !strings.Contains(n, "\\") {
		return n
	}
	return "/status"
}

func (s *Server) loginForm(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", s.newPage(r, nil, "Sign in", "", loginData{s.loginToken(), safeNext(r.URL.Query().Get("next"))}))
}

func (s *Server) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.errorPage(w, r, nil, http.StatusBadRequest, "The form could not be read.")
		return
	}
	next := safeNext(r.PostForm.Get("next"))
	retry := func(code int, msg string) {
		p := s.newPage(r, nil, "Sign in", "", loginData{s.loginToken(), next})
		p.Err = msg
		s.render(w, code, "login", p)
	}
	if !s.loginTokenOK(r.PostForm.Get("csrf")) {
		retry(http.StatusForbidden, "The login form expired. Please try again.")
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !s.limiter.allow(ip) {
		retry(http.StatusTooManyRequests, "Too many failed attempts. Wait a few minutes and try again.")
		return
	}
	name := r.PostForm.Get("user")
	pass := r.PostForm.Get("password")
	var found *config.User
	cfg := s.app.Config()
	for i := range cfg.Users {
		if cfg.Users[i].Name == name {
			found = &cfg.Users[i]
		}
	}
	hash := dummyHash
	if found != nil {
		hash = found.PasswordHash
	}
	ok := config.CheckPassword(hash, pass)
	if found == nil || !ok {
		s.limiter.fail(ip)
		_ = s.app.Store.AddAudit(name, "login.failed", "", "from "+ip)
		retry(http.StatusUnauthorized, "Wrong user name or password.")
		return
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		retry(http.StatusInternalServerError, "Could not create a session.")
		return
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	now := time.Now()
	if err := s.app.Store.CreateSession(store.Session{ID: hashToken(tok), User: found.Name, CreatedAt: now, ExpiresAt: now.Add(sessionTTL)}); err != nil {
		s.app.Logf("create session: %v", err)
		retry(http.StatusInternalServerError, "Could not create a session.")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true, Secure: isHTTPS(r),
		SameSite: http.SameSiteLaxMode, Expires: now.Add(sessionTTL),
	})
	_ = s.app.Store.AddAudit(found.Name, "login", "", "from "+ip)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, u *userCtx) {
	_ = s.app.Store.DeleteSession(u.sessionID)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", HttpOnly: true, Secure: isHTTPS(r), SameSite: http.SameSiteLaxMode, MaxAge: -1})
	_ = s.app.Store.AddAudit(u.Name, "logout", "", "")
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// limiter slows down password guessing: 8 failures per address in 10 minutes.
type limiter struct {
	mu       sync.Mutex
	failures map[string][]time.Time
}

func newLimiter() *limiter { return &limiter{failures: map[string][]time.Time{}} }

func (l *limiter) recent(ip string) []time.Time {
	cut := time.Now().Add(-10 * time.Minute)
	var keep []time.Time
	for _, t := range l.failures[ip] {
		if t.After(cut) {
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		delete(l.failures, ip)
	} else {
		l.failures[ip] = keep
	}
	return keep
}

func (l *limiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(ip)) < 8
}

func (l *limiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failures[ip] = append(l.recent(ip), time.Now())
}
