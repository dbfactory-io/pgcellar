package web

// Every user-visible label that is not part of a sentence goes through here,
// so that a French (or other) catalogue can be added without touching the
// templates. Missing keys fall back to the key itself.
var messages = map[string]string{
	"nav.status":        "Status",
	"nav.servers":       "Servers",
	"nav.runs":          "Runs",
	"nav.history":       "History",
	"nav.audit":         "Audit",
	"nav.logout":        "Log out",
	"dev.banner":        "Development mode: fake Barman, temporary data, clock frozen at the fixtures time, login admin / admin.",
	"login.title":       "Sign in",
	"btn.login":         "Sign in",
	"btn.create":        "Create server",
	"btn.save":          "Save changes",
	"btn.add_server":    "Add a server",
	"btn.run_check":     "Run check now",
	"btn.backup_now":    "Backup now",
	"btn.switch_wal":    "Switch WAL",
	"status.title":      "Status",
	"status.wal":        "WAL",
	"wal.streaming":     "Streaming",
	"wal.live":          "live",
	"wal.stopped":       "stopped",
	"wal.not_used":      "not used (archive_command)",
	"wal.data":          "last data",
	"wal.filled":        "Last filled WAL file",
	"wal.filled_full":   "Last filled WAL file",
	"wal.filled_help":   "A WAL file is 16 MB; a quiet server fills one slowly, the nightly backup closes one.",
	"checks.ok_summary": "checks OK",
	"checks.none":       "No check in the cache yet. Use \"Run check now\" for a live one.",
	"run.also_said":     "Barman also said:",
	"servers.title":     "Servers",
	"servers.new":       "Add a server",
	"runs.title":        "Runs",
	"runs.empty":        "No run yet. Only backups started from pgCellar (this page, or POST /api/v1/servers/<name>/backup) are listed here; backups taken with the barman command appear in each server's page.",
	"history.title":     "Configuration history",
	"audit.title":       "Audit log",

	// Look and feel: the status headline, empty states, small celebrations.
	"motto":                 "The barman serves, the cellar keeps.",
	"level.ok":              "Healthy",
	"level.warn":            "Attention",
	"level.fail":            "Failing",
	"status.ok":             "Everything is fine ✅",
	"status.warn_one":       "1 server needs attention ⚠️",
	"status.warn_many":      "%d servers need attention ⚠️",
	"status.fail_one":       "1 server is failing ❌ — start there.",
	"status.fail_many":      "%d servers are failing ❌ — start there.",
	"status.also_warn_one":  "1 more server needs attention.",
	"status.also_warn_many": "%d more servers need attention.",
	"status.refreshing":     "🔄 Showing the last known state while pgCellar refreshes.",
	"status.none":           "No server to watch yet 🍷",
	"status.none_hint":      "Add a server and pgCellar will tell you, in plain words, whether its backups are fine.",
	"status.last_backup":    "Last backup finished",
	"status.no_backup":      "No backup yet",
	"unit.backup":           "backup",
	"unit.backups":          "backups",
	"empty.cellar":          "Nothing in the cellar yet 🍷",
	"empty.cellar_hint":     "The first backup will show up here.",
	"runs.empty_fun":        "No run yet. Pour the first one?",
	"run.done":              "Backup done in",
	"run.stocked":           "The cellar is stocked ✨",
	"run.failed_hint":       "Read the output below, then run the check on the server page.",
	"busy.checking":         "Checking…",
	"busy.backup":           "Starting the backup…",
	"busy.wal":              "Switching the WAL file…",
	"section.overview":      "Overview",
	"section.backups":       "Backups",
	"section.checks":        "Checks",
	"section.config":        "Configuration",
	"section.runs":          "Recent runs",
	"checks.hint":           "The list comes from the last refresh. Use \"Run check now\" for a live check.",

	// Backup deletion.
	"btn.delete":          "Delete",
	"btn.delete_ask":      "Delete…",
	"btn.keep":            "Keep it",
	"backup.delete_ask_a": "Delete backup",
	"backup.delete_ask_b": "?",
	"backup.delete_warn":  "This cannot be undone.",
}

// tr returns the text for a key.
func tr(key string) string {
	if v, ok := messages[key]; ok {
		return v
	}
	return key
}
