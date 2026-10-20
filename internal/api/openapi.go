package api

import "net/http"

type m = map[string]any

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
					"problems": m{"type": "array", "items": m{"type": "string"}}, "error": m{"type": "string"}, "updated_at": m{"type": "string", "format": "date-time"},
				}},
				"Check": m{"type": "object", "properties": m{"name": m{"type": "string"}, "display": m{"type": "string"}, "ok": m{"type": "boolean"}, "what": m{"type": "string"}, "fix": m{"type": "string"}}},
				"Backup": m{"type": "object", "properties": m{
					"id": m{"type": "string"}, "status": m{"type": "string"}, "begin_time": m{"type": "string", "format": "date-time"}, "end_time": m{"type": "string", "format": "date-time"},
					"size_bytes": m{"type": "integer"}, "wal_size_bytes": m{"type": "integer"}, "pg_version": m{"type": "string"},
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
