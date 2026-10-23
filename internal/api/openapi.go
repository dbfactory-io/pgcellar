package api

import "net/http"

type m = map[string]any

var dt = m{"type": "string", "format": "date-time"}

func ref(name string) m { return m{"$ref": "#/components/schemas/" + name} }

func dataOf(schema m) m {
	return m{"type": "object", "properties": m{"data": schema}}
}

func resp(desc string, schema m) m {
	r := m{"description": desc}
	if schema != nil {
		r["content"] = m{"application/json": m{"schema": schema}}
	}
	return r
}

var errResp = resp("Error", ref("Error"))

func op(summary, role string, extra m) m {
	o := m{
		"summary":     summary,
		"description": "Minimum role: " + role + ".",
		"security":    []m{{"bearer": []string{}}},
		"responses": m{
			"401": errResp,
			"403": errResp,
		},
	}
	for k, v := range extra {
		if k == "responses" {
			for c, r := range v.(m) {
				o["responses"].(m)[c] = r
			}
			continue
		}
		o[k] = v
	}
	return o
}

func nameParam() m {
	return m{"name": "name", "in": "path", "required": true, "schema": m{"type": "string"}}
}

func recoveryParam() m {
	return m{"name": "id", "in": "path", "required": true, "schema": m{"type": "integer"}}
}

func body(schema string) m {
	return m{"required": true, "content": m{"application/json": m{"schema": ref(schema)}}}
}

// openapi is written by hand and kept next to the routes of Handler.
func (a *API) openapi(w http.ResponseWriter, r *http.Request) {
	server := []m{nameParam()}
	doc := m{
		"openapi": "3.0.3",
		"info": m{
			"title":       "pgCellar API",
			"version":     "1",
			"description": "JSON API of pgCellar. Authenticate with 'Authorization: Bearer <key>'. Answers are {\"data\": ...} or {\"error\": \"...\"}.",
		},
		"servers": []m{{"url": "/api/v1"}},
		"paths": m{
			"/servers": m{
				"get":  op("List servers. Cached, refreshed every status_interval; POST /check for a live check. Never runs Barman.", "viewer", m{"responses": m{"200": resp("Servers", dataOf(m{"type": "array", "items": ref("ServerDetail")}))}}),
				"post": op("Add a server (writes barman.d and pgcellar.yaml, commits)", "admin", m{"requestBody": body("ServerInput"), "responses": m{"201": resp("Created", nil), "409": errResp, "422": errResp}}),
			},
			"/servers/{name}": m{
				"get": op("Show a server. Cached, refreshed every status_interval; POST /check for a live check. Never runs Barman.", "viewer", m{"parameters": server, "responses": m{"200": resp("Server", dataOf(ref("ServerDetail"))), "404": errResp}}),
				"put": op("Change a server; fields left out keep their value (writes the files and commits)", "admin", m{"parameters": server, "requestBody": body("ServerInput"), "responses": m{"200": resp("Saved", nil), "404": errResp, "409": errResp, "422": errResp}}),
			},
			"/servers/{name}/setup": m{
				"get": op("Server setup: what pgCellar read in the PostgreSQL settings of the server and what to change there (fix is the exact SQL or line; restart tells whether a restart is needed, else a reload is enough). Cached, read again every setup_interval and by POST /check; never runs a command. level is unknown until the first read. Warnings do not change the health level of the server, errors do.", "viewer", m{"parameters": server, "responses": m{"200": resp("Setup", dataOf(ref("Setup"))), "404": errResp}}),
			},
			"/servers/{name}/timeline": m{
				"get": op("Timeline: what the server can be restored to, built on the fly from Barman's own files (meta/*-backup.info, wals/*-xlog.db, wals/*.history; a few milliseconds, never runs Barman). earliest_restore_point is the end_time of the oldest DONE backup whose WAL chain is unbroken from its begin_wal up to the last filled WAL file; latest_restore_point is the last data received while streaming is live, else the last filled WAL file. When the metadata cannot be read, available is false and unavailable says why.", "viewer", m{"parameters": server, "responses": m{"200": resp("Timeline", dataOf(ref("Timeline"))), "404": errResp}}),
			},
			"/servers/{name}/check": m{
				"post": op("Run barman check now and explain each line", "operator", m{"parameters": server, "responses": m{"200": resp("Checks", dataOf(m{"type": "object", "properties": m{"ok": m{"type": "boolean"}, "checks": m{"type": "array", "items": ref("Check")}}})), "404": errResp, "502": errResp}}),
			},
			"/servers/{name}/backup": m{
				"post": op("Start a backup in the background", "operator", m{"parameters": server, "responses": m{"202": resp("Accepted: follow the run with GET /runs/{id}", dataOf(m{"type": "object", "properties": m{"run_id": m{"type": "integer"}, "status": m{"type": "string"}, "location": m{"type": "string"}}})), "404": errResp, "409": errResp}}),
			},
			"/servers/{name}/switch-wal": m{
				"post": op("Close and archive the current WAL segment", "operator", m{"parameters": server, "responses": m{"200": resp("Done", nil), "404": errResp, "502": errResp}}),
			},
			"/servers/{name}/backups": m{
				"get": op("List the backups of a server, newest first. Cached, refreshed every status_interval; POST /check for a live check. 503 with Retry-After while the first refresh is pending.", "viewer", m{"parameters": server, "responses": m{"200": resp("Backups", dataOf(m{"type": "array", "items": ref("Backup")})), "404": errResp, "503": errResp}}),
			},
			"/servers/{name}/backups/{id}": m{
				"delete": op("Delete one complete backup (barman delete). Barman refuses to go below minimum_redundancy: its message comes back in a 409. Audited as backup.delete.", "admin", m{"parameters": []m{nameParam(), {"name": "id", "in": "path", "required": true, "schema": m{"type": "string", "pattern": "^[0-9]{8}T[0-9]{6}$"}}}, "responses": m{"204": resp("Deleted", nil), "404": errResp, "409": errResp, "502": errResp}}),
			},
			"/servers/{name}/recoveries": m{
				"post": op("Restore a backup to a directory (barman recover, local) and optionally build a .tar.gz, in the background. Follow the run with GET /runs/{id}; the result then shows in GET /servers/{name}/recoveries. 422 when the point in time is before the end of the backup.", "operator", m{"parameters": server, "requestBody": body("RecoveryInput"), "responses": m{"202": resp("Accepted: follow the run with GET /runs/{id}", dataOf(m{"type": "object", "properties": m{"run_id": m{"type": "integer"}, "status": m{"type": "string"}, "location": m{"type": "string"}}})), "404": errResp, "409": errResp, "422": errResp}}),
				"get":  op("Restores of a server, newest first", "viewer", m{"parameters": server, "responses": m{"200": resp("Recoveries", dataOf(m{"type": "array", "items": ref("Recovery")})), "404": errResp}}),
			},
			"/recoveries/{id}": m{
				"get":    op("One restore", "viewer", m{"parameters": []m{recoveryParam()}, "responses": m{"200": resp("Recovery", dataOf(ref("Recovery"))), "404": errResp}}),
				"delete": op("Delete a restore: its directory, its archive and its record. Audited as recovery.delete.", "admin", m{"parameters": []m{recoveryParam()}, "responses": m{"204": resp("Deleted", nil), "404": errResp}}),
			},
			"/recoveries/{id}/download": m{
				"get": op("Download the .tar.gz of a restore (Content-Disposition: attachment, byte ranges supported). The archive holds a copy of the database: operator role. 404 when there is no archive, 413 when it exceeds recoveries.max_download_bytes (read it from the host path instead).", "operator", m{"parameters": []m{recoveryParam()}, "responses": m{"200": m{"description": "The archive", "content": m{"application/gzip": m{"schema": m{"type": "string", "format": "binary"}}}}, "404": errResp, "413": errResp}}),
			},
			"/servers/{name}/drills": m{
				"post": op("Start a restore drill in the background: restore a backup into a throwaway PostgreSQL of the same major version under recoveries.dir, wait for the end of the recovery, check what is inside, stop it, write a PDF report. The body is optional (default: the latest DONE backup, target latest). Follow the run with GET /runs/{id}; the result then shows in GET /servers/{name}/drills. 409 while another drill, or a backup of this server, runs.", "operator", m{"parameters": server, "requestBody": m{"required": false, "content": m{"application/json": m{"schema": ref("DrillInput")}}}, "responses": m{"202": resp("Accepted: follow the run with GET /runs/{id}", dataOf(m{"type": "object", "properties": m{"run_id": m{"type": "integer"}, "status": m{"type": "string"}, "location": m{"type": "string"}}})), "404": errResp, "409": errResp, "422": errResp}}),
				"get":  op("Drills of a server, newest first (?limit=, default 50)", "viewer", m{"parameters": server, "responses": m{"200": resp("Drills", dataOf(m{"type": "array", "items": ref("Drill")})), "404": errResp}}),
			},
			"/drills": m{
				"get": op("Drills of every server, newest first (?server=, ?limit=)", "viewer", m{"parameters": []m{{"name": "server", "in": "query", "schema": m{"type": "string"}}, {"name": "limit", "in": "query", "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Drills", dataOf(m{"type": "array", "items": ref("Drill")}))}}),
			},
			"/drills/{id}": m{
				"get": op("One drill with its steps, databases and the sentence of the report", "viewer", m{"parameters": []m{recoveryParam()}, "responses": m{"200": resp("Drill", dataOf(ref("Drill"))), "404": errResp}}),
			},
			"/drills/{id}/report": m{
				"get": op("Download the PDF report of a drill (operator role). 404 when no report was written.", "operator", m{"parameters": []m{recoveryParam()}, "responses": m{"200": m{"description": "The report", "content": m{"application/pdf": m{"schema": m{"type": "string", "format": "binary"}}}}, "404": errResp}}),
			},
			"/status": m{
				"get": op("Cached status of every server (refreshed every status interval)", "viewer", m{"responses": m{"200": resp("Status", dataOf(m{"type": "object", "properties": m{"servers": m{"type": "array", "items": ref("ServerState")}, "updated_at": m{"type": "string", "format": "date-time"}}}))}}),
			},
			"/runs": m{
				"get": op("Recent runs, newest first", "viewer", m{"parameters": []m{{"name": "server", "in": "query", "schema": m{"type": "string"}}, {"name": "limit", "in": "query", "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Runs", dataOf(m{"type": "array", "items": ref("Run")}))}}),
			},
			"/runs/{id}": m{
				"get": op("One run with its output", "viewer", m{"parameters": []m{{"name": "id", "in": "path", "required": true, "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Run", dataOf(ref("Run"))), "404": errResp}}),
			},
			"/history": m{
				"get": op("Git history of the configuration; with ?diff=<hash>&repo=config|barman, the patch of one commit", "viewer", m{"parameters": []m{{"name": "limit", "in": "query", "schema": m{"type": "integer"}}, {"name": "diff", "in": "query", "schema": m{"type": "string"}}, {"name": "repo", "in": "query", "schema": m{"type": "string", "enum": []string{"config", "barman"}}}}, "responses": m{"200": resp("History", nil)}}),
			},
			"/audit": m{
				"get": op("Audit log", "admin", m{"parameters": []m{{"name": "limit", "in": "query", "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Audit entries", nil)}}),
			},
			"/alerts": m{
				"get": op("Alert channels (no secrets: only whether each environment variable is set) and the journal of the latest messages", "admin", m{"parameters": []m{{"name": "limit", "in": "query", "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Channels and journal", nil)}}),
			},
			"/alerts/test": m{
				"post": op("Send a test message to one channel (JSON body {\"channel\": \"telegram\"}) or to all when the body is empty; audited", "admin", m{"responses": m{"200": resp("One result per channel", nil), "404": errResp, "409": errResp}}),
			},
			"/license": m{
				"get": op("The licence in force: edition, licensee, servers allowed and used, expiry, state", "viewer", m{"responses": m{"200": resp("Licence", nil)}}),
			},
			"/vault": m{
				"get": op("The off-site copy: settings (secret key never shown, only secret_key_set), Object Lock rule of the bucket (lock, lock_text), quota and use (quota_bytes, used_bytes, noncurrent_bytes kept by versioning, used_percent, quota_level ok|warn|fail), whether the recovery key exists and was downloaded, the last connection test, the status of every server (servers[].status: copied, last copy, last_ok_at, fail_streak, used_bytes, stale, why, text) and the queue. Read from pgcellar.yaml and SQLite only.", "viewer", m{"responses": m{"200": resp("Vault", dataOf(ref("Vault")))}}),
				"put": op("Change the vault settings; fields left out keep their value. secret_key is written into vault.secret_key_file (0600), never into pgcellar.yaml. Commits pgcellar.yaml.", "admin", m{"requestBody": body("VaultInput"), "responses": m{"200": resp("Saved", nil), "422": errResp}}),
			},
			"/vault/test": m{
				"post": op("Test the bucket: restic runs, a small object is written, read back and removed, the Object Lock rule is read. Empty body = the saved settings; a VaultInput body tests those values over the saved ones without saving them. 502 when a step failed (the body still carries the steps). Audited as vault.test.", "admin", m{"requestBody": m{"required": false, "content": m{"application/json": m{"schema": ref("VaultInput")}}}, "responses": m{"200": resp("Test result", dataOf(ref("VaultTest"))), "502": resp("A step failed", dataOf(ref("VaultTest"))), "422": errResp}}),
			},
			"/vault/copy": m{
				"post": op("Queue a copy of one server now. Copies run one at a time, in the background; follow it with GET /vault/copies?server=. Audited as vault.copy.", "operator", m{"requestBody": m{"required": true, "content": m{"application/json": m{"schema": m{"type": "object", "required": []string{"server"}, "properties": m{"server": m{"type": "string"}}}}}}, "responses": m{"202": resp("Queued", dataOf(m{"type": "object", "properties": m{"server": m{"type": "string"}, "position": m{"type": "integer"}, "running": m{"type": "boolean"}}})), "404": errResp, "409": errResp}}),
			},
			"/vault/copies": m{
				"get": op("The off-site copies, newest first (?server=, ?limit=, default 50), without their journal.", "viewer", m{"parameters": []m{{"name": "server", "in": "query", "schema": m{"type": "string"}}, {"name": "limit", "in": "query", "schema": m{"type": "integer"}}}, "responses": m{"200": resp("Copies", dataOf(m{"type": "array", "items": ref("VaultCopy")}))}}),
			},
			"/vault/copies/{id}": m{
				"get": op("One copy with its journal (the restic commands and what they said; never a secret).", "viewer", m{"parameters": []m{recoveryParam()}, "responses": m{"200": resp("Copy", dataOf(ref("VaultCopy"))), "404": errResp}}),
			},
			"/vault/recovery-key": m{
				"get": op("The recovery key, text/plain: the restic password of the copies and how to read them without pgCellar. Generated when no copy ran yet. Every download is audited as vault.key.download. Keep it offline: without it the copies cannot be read.", "admin", m{"responses": m{"200": m{"description": "Recovery key", "content": m{"text/plain": m{"schema": m{"type": "string"}}}}, "409": errResp}}),
			},
			"/updates": m{
				"get": op("What the last release check found (current, latest, available, checked_at, published_at, notes_url, manifest, error) and the updates settings (check, interval, auto, window, in_window, docker), plus self_update on a binary installation (enabled, current, refused_reason, last_attempt, last_error, failed_versions). The check sends nothing but the running version, in its User-Agent.", "viewer", m{"responses": m{"200": resp("Update status", nil)}}),
			},
			"/updates/check": m{
				"post": op("Check for a new release now (GET on updates.source, then on the manifest of the release). Audited as updates.check. 502 when the check failed; the body then carries both error and data.", "operator", m{"responses": m{"200": resp("Update status", nil), "502": errResp}}),
			},
			"/updates/ready": m{
				"get": op("For the updater on the Docker host: 200 {\"ready\": true} when no backup, restore or drill runs and the clock of the server is inside updates.window; otherwise 409 {\"ready\": false, \"reason\": \"...\"}. ?now=1 ignores the window. Not wrapped in data.", "operator", m{"parameters": []m{{"name": "now", "in": "query", "schema": m{"type": "string", "enum": []string{"1"}}}}, "responses": m{"200": resp("Ready", m{"type": "object", "properties": m{"ready": m{"type": "boolean"}, "window": m{"type": "string"}}}), "409": resp("Not now", m{"type": "object", "properties": m{"ready": m{"type": "boolean"}, "reason": m{"type": "string"}, "window": m{"type": "string"}}})}}),
			},
			"/ssh/authorized-keys": m{
				"get": op("Keys allowed to push WAL files to the SSH companion (the file ~barman/.ssh/authorized_keys, read at each call)", "admin", m{"responses": m{"200": resp("Authorised keys", m{"type": "object", "properties": m{"data": m{"type": "array", "items": ref("AuthorizedKey")}}})}}),
				"post": op("Authorise the public key of the postgres user of a PostgreSQL server; the line is restricted to `barman put-wal <server>`", "admin", m{
					"requestBody": m{"required": true, "content": m{"application/json": m{"schema": m{"type": "object", "required": []string{"server", "public_key"}, "properties": m{"server": m{"type": "string"}, "public_key": m{"type": "string", "description": "One line: ssh-ed25519, ssh-rsa or ecdsa-sha2-* key, no options"}}}}}},
					"responses":   m{"201": resp("Key authorised", dataOf(ref("AuthorizedKey"))), "404": errResp, "409": resp("Key already authorised", ref("Error")), "422": errResp}}),
				"delete": op("Remove an authorised key", "admin", m{"parameters": []m{{"name": "fingerprint", "in": "query", "required": true, "schema": m{"type": "string"}, "description": "SHA256:... as listed by GET"}}, "responses": m{"204": resp("Removed", nil), "404": errResp}}),
			},
			"/openapi.json": m{
				"get": m{"summary": "This document", "responses": m{"200": resp("OpenAPI 3 description", nil)}},
			},
		},
		"components": m{
			"securitySchemes": m{"bearer": m{"type": "http", "scheme": "bearer"}},
			"schemas": m{
				"AuthorizedKey": m{"type": "object", "properties": m{"server": m{"type": "string"}, "type": m{"type": "string"}, "fingerprint": m{"type": "string"}, "comment": m{"type": "string"}, "managed": m{"type": "boolean", "description": "the line carries the put-wal restriction written by pgCellar"}}},
				"Error":         m{"type": "object", "properties": m{"error": m{"type": "string"}, "problems": m{"type": "array", "items": m{"type": "string"}}}},
				"ServerInput": m{"type": "object", "properties": m{
					"name": m{"type": "string"}, "label": m{"type": "string"}, "description": m{"type": "string"},
					"backup_method": m{"type": "string", "enum": []string{"postgres", "rsync"}},
					"wal_method":    m{"type": "string", "enum": []string{"streaming", "archive_command", "both"}, "description": "How WAL files reach Barman: streaming_archiver on, archiver on, or both. Omitted = unchanged (new server: streaming for postgres, archive_command for rsync)."},
					"conninfo":      m{"type": "string"}, "streaming_conninfo": m{"type": "string"}, "slot_name": m{"type": "string"}, "ssh_command": m{"type": "string"},
					"retention": m{"type": "object", "properties": m{"recovery_window_days": m{"type": "integer"}, "redundancy": m{"type": "integer"}, "minimum_redundancy": m{"type": "integer"}}},
					"schedule":  m{"type": "object", "properties": m{"backup": m{"type": "string", "example": "daily 02:00"}, "enabled": m{"type": "boolean"}}},
				}},
				"ServerDetail": m{"allOf": []m{ref("ServerInput"), {"type": "object", "properties": m{
					"has_file": m{"type": "boolean"}, "in_barman": m{"type": "boolean"}, "file": m{"type": "string"}, "schedule_human": m{"type": "string"},
					"problems": m{"type": "array", "items": m{"type": "string"}}, "state": ref("ServerState"),
				}}}},
				"ServerState": m{"type": "object", "properties": m{
					"name": m{"type": "string"}, "level": m{"type": "string", "enum": []string{"ok", "warn", "fail"}},
					"last_backup_id": m{"type": "string"}, "last_backup_end": m{"type": "string", "format": "date-time"}, "last_backup_size": m{"type": "integer"},
					"last_backup_age_seconds": m{"type": "integer"}, "last_filled_wal": m{"type": "string", "description": "Name of the last filled WAL file (16 MB; a quiet server fills one slowly)"}, "last_filled_wal_time": m{"type": "string", "format": "date-time"}, "last_filled_wal_age_seconds": m{"type": "integer"},
					"last_wal": m{"type": "string", "format": "date-time", "deprecated": true, "description": "Alias of last_filled_wal_time"}, "last_wal_age_seconds": m{"type": "integer", "deprecated": true, "description": "Alias of last_filled_wal_age_seconds"},
					"streaming_monitored": m{"type": "boolean"}, "stale": m{"type": "boolean", "description": "Reloaded from the database after a restart; the first refresh has not completed yet"},
					"streaming_live": m{"type": "boolean", "description": "The Barman WAL receiver runs"}, "last_data_received": m{"type": "string", "format": "date-time"}, "last_data_received_age_seconds": m{"type": "integer"},
					"notes":        m{"type": "array", "items": m{"type": "string"}},
					"backup_count": m{"type": "integer"}, "checks": m{"type": "array", "items": ref("Check")}, "check_ok": m{"type": "boolean"},
					"problems": m{"type": "array", "items": m{"type": "string"}}, "error": m{"type": "string"},
					"setup": m{"type": "array", "items": ref("SetupCheck")}, "setup_level": m{"type": "string", "enum": []string{"ok", "warning", "error"}}, "setup_issues": m{"type": "integer"}, "setup_at": m{"type": "string", "format": "date-time"},
					"updated_at": m{"type": "string", "format": "date-time"},
					"storage":    ref("Storage"),
					"drill":      m{"type": "object", "description": "Restore drills: scheduled, schedule, next, last {id, ok, backup_id, finished_at, step_failed, has_report, summary, age_seconds}, last_ok_at, last_ok_age_seconds, running, stale (the schedule is set and the last drill failed, or none succeeded for drills.max_age: the server is then warn), text (\"Last drill: 3 d ago\" + a check mark or a cross), why"},
				}},
				"Storage": m{"type": "object", "description": "What the server takes on disk and what is left; measured at each refresh, never by running a command", "properties": m{
					"backup_bytes": m{"type": "integer", "description": "sum of size of the DONE backups"}, "backup_disk_bytes": m{"type": "integer", "description": "sum of deduplicated_size (size when Barman does not say)"}, "backups": m{"type": "integer"},
					"wal_bytes": m{"type": "integer", "description": "sum of the sizes in the WAL catalogue"}, "wal_files": m{"type": "integer"},
					"recoveries_bytes": m{"type": "integer", "description": "restores of the server under recoveries.dir"}, "total_bytes": m{"type": "integer", "description": "backup_disk_bytes + wal_bytes + recoveries_bytes"},
					"added_per_day": m{"type": "integer", "description": "WAL and backups of the last 7 days (or since the oldest data, at least 1 day) per day"}, "days_until_full": m{"type": "integer", "description": "free space / added_per_day, present only when under 90"},
					"fs": ref("Filesystem"), "recoveries_fs": ref("Filesystem"), "error": m{"type": "string"},
				}},
				"Filesystem": m{"type": "object", "properties": m{
					"path": m{"type": "string"}, "total_bytes": m{"type": "integer"}, "free_bytes": m{"type": "integer", "description": "available to a normal user"}, "used_bytes": m{"type": "integer"},
					"used_percent": m{"type": "number"}, "level": m{"type": "string", "enum": []string{"ok", "warn", "fail"}, "description": "against alerts.min_free_percent and alerts.min_free_bytes"},
				}},
				"Timeline": m{"type": "object", "properties": m{
					"server": m{"type": "string"}, "available": m{"type": "boolean"}, "unavailable": m{"type": "string", "description": "\"unavailable: <reason>\" when available is false"},
					"now": dt, "axis_start": dt, "axis_end": dt,
					"retention":              m{"type": "object", "properties": m{"recovery_window_days": m{"type": "integer"}, "redundancy": m{"type": "integer"}, "minimum_redundancy": m{"type": "integer"}, "start": dt}},
					"backups":                m{"type": "array", "items": ref("TimelineBackup")},
					"intervals":              m{"type": "array", "description": "WAL between the begin of one DONE backup and the next (the last one runs to the newest data)", "items": ref("TimelineInterval")},
					"timelines":              m{"type": "array", "description": "PostgreSQL timelines seen in the WAL, with their fork point", "items": ref("WALTimeline")},
					"wal_gaps":               m{"type": "array", "items": m{"type": "object", "properties": m{"timeline": m{"type": "integer"}, "from": dt, "to": dt, "missing_wal_files": m{"type": "integer"}}}},
					"earliest_restore_point": dt, "earliest_backup": m{"type": "string"}, "latest_restore_point": dt, "latest_source": m{"type": "string", "enum": []string{"streaming", "last_filled_wal"}},
					"last_filled_wal": m{"type": "string"}, "last_filled_wal_time": dt, "last_data_received": dt, "next_backup": dt,
					"totals": m{"type": "object", "properties": m{"backups": m{"type": "integer"}, "full_backups": m{"type": "integer"}, "incremental_backups": m{"type": "integer"}, "backup_bytes": m{"type": "integer"}, "wal_files": m{"type": "integer"}, "wal_bytes": m{"type": "integer"}, "wal_gaps": m{"type": "integer"}, "missing_wal_files": m{"type": "integer"}, "failed_backups": m{"type": "integer"}}},
					"notes":  m{"type": "array", "items": m{"type": "string"}},
				}},
				"TimelineBackup": m{"type": "object", "properties": m{
					"id": m{"type": "string"}, "status": m{"type": "string"}, "begin_time": dt, "end_time": dt, "size": m{"type": "integer"}, "deduplicated_size": m{"type": "integer"},
					"timeline": m{"type": "integer"}, "begin_wal": m{"type": "string"}, "end_wal": m{"type": "string"}, "parent_backup_id": m{"type": "string"},
					"restorable": m{"type": "boolean", "description": "DONE and the WAL chain is unbroken up to the last filled WAL file"},
				}},
				"TimelineInterval": m{"type": "object", "properties": m{
					"from": dt, "to": dt, "from_backup": m{"type": "string"}, "to_backup": m{"type": "string"}, "wal_files": m{"type": "integer"}, "wal_bytes": m{"type": "integer", "description": "Bytes as stored in xlog.db"},
					"continuous": m{"type": "boolean"}, "missing_wal_files": m{"type": "integer"},
				}},
				"WALTimeline": m{"type": "object", "properties": m{
					"id": m{"type": "integer"}, "parent": m{"type": "integer"}, "branched_at": dt, "switch_lsn": m{"type": "string"}, "reason": m{"type": "string"},
					"first_wal_time": dt, "last_wal_time": dt, "wal_files": m{"type": "integer"}, "wal_bytes": m{"type": "integer"}, "current": m{"type": "boolean"},
				}},
				"Setup": m{"type": "object", "properties": m{
					"server": m{"type": "string"}, "level": m{"type": "string", "enum": []string{"ok", "warning", "error", "unknown"}},
					"at":     m{"type": "string", "format": "date-time", "description": "When the PostgreSQL settings were read"},
					"checks": m{"type": "array", "items": ref("SetupCheck")},
				}},
				"SetupCheck": m{"type": "object", "properties": m{
					"key": m{"type": "string"}, "name": m{"type": "string"}, "level": m{"type": "string", "enum": []string{"ok", "info", "warning", "error"}},
					"value": m{"type": "string"}, "expected": m{"type": "string"}, "why": m{"type": "string"},
					"fix":     m{"type": "string", "description": "What to run or write on the PostgreSQL server"},
					"restart": m{"type": "boolean", "description": "The fix needs a restart of PostgreSQL; otherwise a reload is enough"},
				}},
				"Check": m{"type": "object", "properties": m{"name": m{"type": "string"}, "display": m{"type": "string"}, "ok": m{"type": "boolean"}, "what": m{"type": "string"}, "fix": m{"type": "string"}}},
				"Backup": m{"type": "object", "properties": m{
					"id": m{"type": "string"}, "status": m{"type": "string"}, "begin_time": m{"type": "string", "format": "date-time"}, "end_time": m{"type": "string", "format": "date-time"},
					"size_bytes": m{"type": "integer"}, "wal_size_bytes": m{"type": "integer"}, "pg_version": m{"type": "string"},
				}},
				"RecoveryInput": m{"type": "object", "required": []string{"backup_id"}, "properties": m{
					"backup_id":   m{"type": "string", "pattern": "^[0-9]{8}T[0-9]{6}$"},
					"target":      m{"type": "string", "enum": []string{"latest", "backup_end", "time"}, "default": "latest", "description": "latest = as far as the archived WAL goes; backup_end = the state at the end of the backup; time = the point in time of target_time"},
					"target_time": m{"type": "string", "example": "2026-10-20 14:30:00+02:00", "description": "With target=time: RFC 3339 or 'YYYY-MM-DD HH:MM:SS+TZ'; without a zone, the time zone of the server. Must not be before the end of the backup."},
					"archive":     m{"type": "boolean", "description": "Also build <directory>.tar.gz"},
				}},
				"Recovery": m{"type": "object", "properties": m{
					"id": m{"type": "integer"}, "server": m{"type": "string"}, "backup_id": m{"type": "string"}, "target": m{"type": "string"}, "target_time": m{"type": "string"},
					"path": m{"type": "string", "description": "Directory inside the container, under recoveries.dir"}, "archive_path": m{"type": "string"},
					"size_bytes": m{"type": "integer"}, "archive_bytes": m{"type": "integer"}, "downloadable": m{"type": "boolean"},
					"run_id": m{"type": "integer"}, "created_at": m{"type": "string", "format": "date-time"}, "actor": m{"type": "string"},
				}},
				"DrillInput": m{"type": "object", "properties": m{
					"backup_id":   m{"type": "string", "pattern": "^[0-9]{8}T[0-9]{6}$", "description": "Default: the latest backup that is DONE"},
					"target":      m{"type": "string", "enum": []string{"latest", "backup_end", "time"}, "default": "latest"},
					"target_time": m{"type": "string", "description": "With target=time"},
				}},
				"Drill": m{"type": "object", "properties": m{
					"id": m{"type": "integer"}, "server": m{"type": "string"}, "backup_id": m{"type": "string"},
					"started_at": m{"type": "string", "format": "date-time"}, "finished_at": m{"type": "string", "format": "date-time"},
					"ok": m{"type": "boolean"}, "step_failed": m{"type": "string", "enum": []string{"recover", "version", "configure", "start", "recovery", "checks", "stop", "checksums", "cleanup"}},
					"run_id": m{"type": "integer"}, "has_report": m{"type": "boolean"},
					"details": m{"type": "object", "description": "summary (the sentence of the report), steps [{key,label,status,millis,note}], databases [{name,size_bytes,tables,rows}], pg_version, lsn, checksums, size_bytes, duration_ms, error, log_tail, fingerprint (SHA-256 of the report content), data_dir (while the restored copy is kept)"},
				}},
				"VaultInput": m{"type": "object", "properties": m{
					"enabled": m{"type": "boolean"}, "endpoint": m{"type": "string", "example": "https://s3.gra.io.cloud.ovh.net"}, "region": m{"type": "string", "example": "gra"},
					"bucket": m{"type": "string"}, "prefix": m{"type": "string", "default": "pgcellar"}, "access_key": m{"type": "string"},
					"secret_key": m{"type": "string", "description": "Written into vault.secret_key_file, never stored in pgcellar.yaml; empty = keep"},
					"quota":      m{"type": "string", "example": "500GB"}, "wal_interval": m{"type": "string", "example": "15m", "description": "0 = only after backups"},
					"max_age": m{"type": "string", "example": "36h"}, "keep_last": m{"type": "integer"}, "keep_daily": m{"type": "integer"}, "keep_weekly": m{"type": "integer"}, "keep_monthly": m{"type": "integer"},
					"drill_source": m{"type": "string", "enum": []string{"local", "vault"}},
				}},
				"Vault": m{"type": "object", "properties": m{
					"config": m{"type": "object"}, "enabled": m{"type": "boolean"}, "secret_key_set": m{"type": "boolean"}, "key_exists": m{"type": "boolean"},
					"key_created_at": dt, "key_downloaded_at": dt, "key_downloaded_by": m{"type": "string"},
					"lock": m{"type": "object", "properties": m{"enabled": m{"type": "boolean"}, "mode": m{"type": "string"}, "days": m{"type": "integer"}}}, "lock_text": m{"type": "string"},
					"quota_bytes": m{"type": "integer"}, "used_bytes": m{"type": "integer"}, "noncurrent_bytes": m{"type": "integer"}, "used_percent": m{"type": "number"}, "quota_level": m{"type": "string"},
					"last_test": ref("VaultTest"), "servers": m{"type": "array", "items": m{"type": "object"}}, "queue": m{"type": "array", "items": m{"type": "string"}}, "drill_source": m{"type": "string"},
				}},
				"VaultTest": m{"type": "object", "properties": m{
					"at": dt, "actor": m{"type": "string"}, "ok": m{"type": "boolean"}, "target": m{"type": "string"}, "error": m{"type": "string"},
					"steps": m{"type": "array", "items": m{"type": "object", "properties": m{"key": m{"type": "string", "enum": []string{"restic", "secret", "write", "read", "delete", "lock"}}, "label": m{"type": "string"}, "ok": m{"type": "boolean"}, "note": m{"type": "string"}, "millis": m{"type": "integer"}}}},
					"lock":  m{"type": "object"},
				}},
				"VaultCopy": m{"type": "object", "properties": m{
					"id": m{"type": "integer"}, "server": m{"type": "string"}, "trigger": m{"type": "string", "enum": []string{"backup", "wal", "manual"}}, "actor": m{"type": "string"},
					"started_at": dt, "finished_at": dt, "running": m{"type": "boolean"}, "ok": m{"type": "boolean"}, "snapshot_id": m{"type": "string", "description": "Empty when nothing changed since the previous copy"},
					"bytes_added": m{"type": "integer"}, "bytes_total": m{"type": "integer"}, "files_new": m{"type": "integer"}, "duration_ms": m{"type": "integer"},
					"error": m{"type": "string"}, "warnings": m{"type": "array", "items": m{"type": "string"}}, "unchanged": m{"type": "boolean"}, "initialised": m{"type": "boolean"},
					"pruned": m{"type": "boolean"}, "prune_error": m{"type": "string"}, "journal": m{"type": "string"},
				}},
				"Run": m{"type": "object", "properties": m{
					"id": m{"type": "integer"}, "server": m{"type": "string"}, "kind": m{"type": "string"}, "status": m{"type": "string", "enum": []string{"running", "ok", "failed"}},
					"started_at": m{"type": "string", "format": "date-time"}, "finished_at": m{"type": "string", "format": "date-time"}, "exit_code": m{"type": "integer"},
					"actor": m{"type": "string"}, "stdout": m{"type": "string"}, "stderr": m{"type": "string"},
				}},
			},
		},
	}
	writeJSON(w, http.StatusOK, doc)
}
