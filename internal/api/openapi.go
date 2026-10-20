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
				"get": op("Timeline: what the server can be restored to, built on the fly from Barman's own files (meta/*-backup.info, wals/*-xlog.db, wals/*.history; a few milliseconds, never runs Barman). earliest_restore_point is the begin_time of the oldest DONE backup whose WAL chain is unbroken from its begin_wal up to the last filled WAL file; latest_restore_point is the last data received while streaming is live, else the last filled WAL file. When the metadata cannot be read, available is false and unavailable says why.", "viewer", m{"parameters": server, "responses": m{"200": resp("Timeline", dataOf(ref("Timeline"))), "404": errResp}}),
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
			"/openapi.json": m{
				"get": m{"summary": "This document", "responses": m{"200": resp("OpenAPI 3 description", nil)}},
			},
		},
		"components": m{
			"securitySchemes": m{"bearer": m{"type": "http", "scheme": "bearer"}},
			"schemas": m{
				"Error": m{"type": "object", "properties": m{"error": m{"type": "string"}, "problems": m{"type": "array", "items": m{"type": "string"}}}},
				"ServerInput": m{"type": "object", "properties": m{
					"name": m{"type": "string"}, "label": m{"type": "string"}, "description": m{"type": "string"},
					"backup_method": m{"type": "string", "enum": []string{"postgres", "rsync"}},
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
