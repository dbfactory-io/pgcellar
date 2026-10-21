package web

import (
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/dbfactory-io/pgcellar/internal/app"
)

// The public demo (pgcellar serve --demo) shows every page and changes
// nothing: this file is the server-side half of that promise. The other half,
// disabled buttons, is cosmetic (web/static/demo.js); what really protects the
// instance is demoGate.

// Demo credentials, also printed on the login page.
const (
	DemoUser     = "demo"
	DemoPassword = "demo"
)

// demoDenied are the GET routes that run a command or hand out an archive;
// they are refused in the demo like any change.
var demoDenied = []*regexp.Regexp{
	regexp.MustCompile(`^/servers/[^/]+/check$`),
	regexp.MustCompile(`^/recoveries/[^/]+/download$`),
}

func (s *Server) canOperate(u *userCtx) bool {
	return s.app.Demo || app.RoleAtLeast(u.Role, app.RoleOperator)
}

func (s *Server) canAdmin(u *userCtx) bool {
	return s.app.Demo || app.RoleAtLeast(u.Role, app.RoleAdmin)
}

// demoAllowed reports whether a request may reach the handlers in the demo.
func demoAllowed(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		for _, re := range demoDenied {
			if re.MatchString(r.URL.Path) {
				return false
			}
		}
		return true
	case http.MethodPost:
		return r.URL.Path == "/login" || r.URL.Path == "/logout"
	}
	return false
}

// demoGate refuses, in the demo, every request that would change something:
// any POST, PUT, PATCH or DELETE except the sign-in and sign-out, and the few
// GET routes that start a command. The answer is 403 with the demo message.
func (s *Server) demoGate(next http.Handler) http.Handler {
	if !s.app.Demo {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if demoAllowed(r) {
			next.ServeHTTP(w, r)
			return
		}
		msg := tr("demo.readonly")
		if len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
			return
		}
		s.errorPage(w, r, s.currentUser(r), http.StatusForbidden, msg)
	})
}
