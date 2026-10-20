package barman

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Parsers for the output of `barman --format json <command>`. The shape was
// read from Barman's JsonOutputWriter (see README.md in this directory). They
// are tolerant on purpose: unknown keys are ignored, numbers may be numbers or
// strings, timestamps may be in several formats, and missing fields are left
// at their zero value. Only a payload that is not a JSON object is an error.

// decodeObject finds the first JSON object in data (Barman may print a
// warning before it) and decodes it.
func decodeObject(data []byte) (map[string]any, error) {
	i := bytes.IndexByte(data, '{')
	if i < 0 {
		return nil, errors.New("no JSON object in barman output")
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(data[i:]))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("invalid JSON in barman output: %w", err)
	}
	return m, nil
}

// messages returns the strings of a "_ERROR", "_WARNING" or "_INFO" list.
func messages(m map[string]any, key string) []string {
	var out []string
	if l, ok := m[key].([]any); ok {
		for _, v := range l {
			out = append(out, asString(v))
		}
	}
	return out
}

// OutputError returns the "_ERROR" messages Barman put in a JSON payload,
// joined, or "" if there are none. Barman reports problems such as an unknown
// server this way.
func OutputError(data []byte) string {
	m, err := decodeObject(data)
	if err != nil {
		return ""
	}
	return strings.Join(messages(m, "_ERROR"), "; ")
}

func asString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case json.Number:
		return x.String()
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func asInt64(v any) int64 {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
	case float64:
		return int64(x)
	case string:
		if i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64); err == nil {
			return i
		}
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return int64(f)
		}
	}
	return 0
}

var timeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02 15:04:05",
	time.ANSIC, // Python ctime(): "Tue Oct 20 02:00:45 2026"
	"Mon Jan _2 15:04:05 2006 MST",
}

// parseTime reads a Barman date. A bare epoch (the *_timestamp keys) and the
// formats Barman prints are accepted; zone-less dates are read in local time,
// which is what Barman's ctime() output means.
func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 1e8 {
		return time.Unix(int64(n), 0)
	}
	for _, l := range timeLayouts {
		if t, err := time.ParseInLocation(l, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// pickTime prefers the epoch key, then the textual one.
func pickTime(m map[string]any, tsKey, textKey string) time.Time {
	if t := parseTime(asString(m[tsKey])); !t.IsZero() {
		return t
	}
	return parseTime(asString(m[textKey]))
}

// idTime reads a backup id such as 20261020T020001 (server local time).
func idTime(id string) time.Time {
	t, err := time.ParseInLocation("20060102T150405", id, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t
}

// pgVersion turns the integer server_version_num Barman prints (150004) into
// "15.4"; text is returned unchanged.
func pgVersion(v any) string {
	s := asString(v)
	n, err := strconv.Atoi(s)
	if err != nil || n < 10000 {
		return s
	}
	if n >= 100000 {
		return fmt.Sprintf("%d.%d", n/10000, n%10000)
	}
	return fmt.Sprintf("%d.%d.%d", n/10000, (n/100)%100, n%100)
}

// ParseListServers reads `list-servers`. Barman 3.20 emits an object indexed
// by server name, {"chutag": {"description": "..."}, ...}, the same with or
// without --minimal. The older {"_INFO": ["name - description", ...]} shape is
// still accepted. Names come back sorted.
func ParseListServers(data []byte) ([]string, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	var names []string
	for k, v := range m {
		if strings.HasPrefix(k, "_") {
			continue
		}
		if _, ok := v.(map[string]any); ok {
			names = append(names, k)
		}
	}
	for _, line := range messages(m, "_INFO") {
		name := strings.TrimSpace(line)
		if i := strings.Index(name, " - "); i >= 0 {
			name = name[:i]
		}
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// ParseCheck reads `check <server>`: {"<server>": {"<check_key>": {"status":
// "OK"|"FAILED", "hint": "..."}}}. Keys come back sorted, as Barman writes
// them. Name is the mangled key (e.g. "wal_archive"); see DisplayName.
func ParseCheck(data []byte, server string) ([]CheckResult, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	srv, ok := m[server].(map[string]any)
	if !ok {
		if msg := strings.Join(messages(m, "_ERROR"), "; "); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("server %q not found in check output", server)
	}
	keys := make([]string, 0, len(srv))
	for k := range srv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []CheckResult
	for _, k := range keys {
		c, ok := srv[k].(map[string]any)
		if !ok {
			continue
		}
		out = append(out, CheckResult{
			Name: k,
			OK:   strings.EqualFold(asString(c["status"]), "OK"),
			Hint: asString(c["hint"]),
		})
	}
	return out, nil
}

// ParseListBackups reads `list-backups <server>`:
// {"<server>": [{"backup_id": ..., "end_time": ..., "size_bytes": ...}]}.
// Entries that are plain strings (--minimal) become backups with only an ID.
// BeginTime is derived from the id, because list-backups does not print it.
func ParseListBackups(data []byte, server string) ([]Backup, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	list, ok := m[server].([]any)
	if !ok {
		if msg := strings.Join(messages(m, "_ERROR"), "; "); msg != "" {
			return nil, errors.New(msg)
		}
		if _, present := m[server]; !present {
			return nil, fmt.Errorf("server %q not found in list-backups output", server)
		}
		return nil, nil
	}
	var out []Backup
	for _, e := range list {
		switch x := e.(type) {
		case string:
			out = append(out, Backup{Server: server, ID: x, BeginTime: idTime(x)})
		case map[string]any:
			id := asString(x["backup_id"])
			out = append(out, Backup{
				Server:    server,
				ID:        id,
				Status:    asString(x["status"]),
				BeginTime: idTime(id),
				EndTime:   pickTime(x, "end_time_timestamp", "end_time"),
				Size:      asInt64(x["size_bytes"]),
				WALSize:   asInt64(x["wal_size_bytes"]),
				Raw:       x,
			})
		}
	}
	return out, nil
}

func sub(m map[string]any, key string) map[string]any {
	s, _ := m[key].(map[string]any)
	return s
}

// ParseShowBackup reads `show-backup <server> <id>`:
// {"<server>": {"backup_id": ..., "status": ..., "base_backup_information":
// {...}, "wal_information": {...}, ...}}.
func ParseShowBackup(data []byte, server string) (*Backup, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	o := sub(m, server)
	if o == nil {
		if msg := strings.Join(messages(m, "_ERROR"), "; "); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("server %q not found in show-backup output", server)
	}
	b := &Backup{
		Server:    server,
		ID:        asString(o["backup_id"]),
		Status:    asString(o["status"]),
		PGVersion: pgVersion(o["postgresql_version"]),
		Raw:       o,
	}
	b.BeginTime = idTime(b.ID)
	if bi := sub(o, "base_backup_information"); bi != nil {
		if t := pickTime(bi, "begin_time_timestamp", "begin_time"); !t.IsZero() {
			b.BeginTime = t
		}
		b.EndTime = pickTime(bi, "end_time_timestamp", "end_time")
		b.Size = asInt64(bi["backup_size_bytes"])
		b.WALSize = asInt64(bi["wal_size_bytes"])
	}
	if b.WALSize == 0 {
		if wi := sub(o, "wal_information"); wi != nil {
			b.WALSize = asInt64(wi["disk_usage_bytes"])
		}
	}
	return b, nil
}

// StatusInfo is the parsed `status <server>` output. Fields holds every line
// Barman printed, keyed by its status code (e.g. "backups_number").
type StatusInfo struct {
	Fields      map[string]StatusField
	Active      bool // false when Barman says the server is inactive or disabled
	BackupCount int
	LastBackup  string // id of the last available backup, "" if none
	LastWAL     string // last archived WAL segment name
	LastWALTime time.Time
}

// StatusField is one line of `barman status`.
type StatusField struct {
	Description string
	Message     string
}

// ParseStatus reads `status <server>`: {"<server>": {"<code>": {"description":
// ..., "message": ...}}}.
func ParseStatus(data []byte, server string) (*StatusInfo, error) {
	m, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	o := sub(m, server)
	if o == nil {
		if msg := strings.Join(messages(m, "_ERROR"), "; "); msg != "" {
			return nil, errors.New(msg)
		}
		return nil, fmt.Errorf("server %q not found in status output", server)
	}
	st := &StatusInfo{Fields: map[string]StatusField{}, Active: true}
	for k, v := range o {
		f, ok := v.(map[string]any)
		if !ok {
			continue
		}
		st.Fields[k] = StatusField{Description: asString(f["description"]), Message: asString(f["message"])}
	}
	if f, ok := st.Fields["active"]; ok && strings.EqualFold(f.Message, "false") {
		st.Active = false
	}
	if f, ok := st.Fields["disabled"]; ok && strings.EqualFold(f.Message, "true") {
		st.Active = false
	}
	st.BackupCount = int(asInt64(st.Fields["backups_number"].Message))
	if id := st.Fields["last_backup"].Message; id != "" && id != "None" && id != "null" {
		st.LastBackup = id
	}
	// "000000010000000000000004, at Tue Oct 20 02:05:10 2026"
	if msg := st.Fields["last_archived_wal"].Message; msg != "" && !strings.HasPrefix(msg, "No WAL") {
		name, when, found := strings.Cut(msg, ", at ")
		st.LastWAL = strings.TrimSpace(name)
		if found {
			st.LastWALTime = parseTime(when)
		}
	}
	return st, nil
}

// ParseDiagnoseWAL reads the archived-WAL part of the global `barman
// diagnose` output. Barman 3.20 prints {"_INFO": ["<JSON document as text>"]}
// where the document holds servers.<name>.wals.last_archived_wal_per_timeline:
// {"<timeline>": {"name": "...", "time": <epoch>, "size": ...}}. It returns
// the segment with the newest archive time across timelines; the name is empty
// and the time zero when nothing was archived yet.
func ParseDiagnoseWAL(data []byte, server string) (string, time.Time, error) {
	m, err := decodeObject(data)
	if err != nil {
		return "", time.Time{}, err
	}
	doc := m
	if msgs := messages(m, "_INFO"); len(msgs) > 0 {
		inner, ierr := decodeObject([]byte(msgs[0]))
		if ierr != nil {
			return "", time.Time{}, ierr
		}
		doc = inner
	}
	o := sub(sub(doc, "servers"), server)
	if o == nil {
		if msg := strings.Join(messages(m, "_ERROR"), "; "); msg != "" {
			return "", time.Time{}, errors.New(msg)
		}
		return "", time.Time{}, fmt.Errorf("server %q not found in diagnose output", server)
	}
	var name string
	var when time.Time
	for _, v := range sub(sub(o, "wals"), "last_archived_wal_per_timeline") {
		w, ok := v.(map[string]any)
		if !ok {
			continue
		}
		t := parseTime(asString(w["time"]))
		if t.IsZero() || !t.After(when) {
			continue
		}
		when, name = t, asString(w["name"])
	}
	return name, when, nil
}
