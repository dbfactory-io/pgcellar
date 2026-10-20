package web

// Every user-visible label that is not part of a sentence goes through here,
// so that a French (or other) catalogue can be added without touching the
// templates. Missing keys fall back to the key itself.
var messages = map[string]string{
	"nav.status":      "Status",
	"nav.servers":     "Servers",
	"nav.runs":        "Runs",
	"nav.history":     "History",
	"nav.audit":       "Audit",
	"nav.logout":      "Log out",
	"dev.banner":      "Development mode: fake Barman, temporary data, clock frozen at the fixtures time, login admin / admin.",
	"login.title":     "Sign in",
	"btn.login":       "Sign in",
	"btn.create":      "Create server",
	"btn.save":        "Save changes",
	"btn.add_server":  "Add a server",
	"btn.run_check":   "Run check now",
	"btn.backup_now":  "Backup now",
	"btn.switch_wal":  "Switch WAL",
	"status.title":    "Status",
	"status.wal":      "WAL",
	"wal.streaming":   "Streaming",
	"wal.live":        "live",
	"wal.stopped":     "stopped",
	"wal.not_used":    "not used (archive_command)",
	"wal.data":        "last data",
	"wal.filled":      "Last filled WAL file",
	"wal.filled_full": "Last filled WAL file (a WAL file is 16 MB; a quiet server fills one slowly, the nightly backup closes one)",
	"servers.title":   "Servers",
	"servers.new":     "Add a server",
	"runs.title":      "Runs",
	"runs.empty":      "No run yet. Only backups started from pgCellar (this page, or POST /api/v1/servers/<name>/backup) are listed here; backups taken with the barman command appear in each server's page.",
	"history.title":   "Configuration history",
	"audit.title":     "Audit log",
}

// tr returns the text for a key.
func tr(key string) string {
	if v, ok := messages[key]; ok {
		return v
	}
	return key
}
