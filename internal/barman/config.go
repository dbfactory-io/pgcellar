package barman

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Barman reads its configuration with Python's configparser: [section]
// headers, "key = value" lines, "#" or ";" comment lines, no quoting, and
// indented lines continue the previous value. This file reads and writes that
// format. Writing is line based: a save only touches the lines whose value
// changed, so comments, blank lines, key order and unknown keys survive.

// knownKeys lists the options typed in Server, in the order used when a key
// has to be appended to a section.
var knownKeys = []string{
	"description",
	"backup_method",
	"conninfo",
	"ssh_command",
	"streaming_conninfo",
	"streaming_archiver",
	"slot_name",
	"archiver",
	"retention_policy",
	"minimum_redundancy",
}

type lineKind int

const (
	kindBlank lineKind = iota
	kindComment
	kindSection
	kindKey
	kindCont // continuation of the previous key
	kindOther
)

type iniLine struct {
	raw   string
	kind  lineKind
	name  string // section name for kindSection
	key   string // lower-cased key for kindKey
	value string // trimmed value for kindKey (continuations appended with "\n")
}

type iniDoc struct {
	lines   []iniLine
	finalNL bool
}

func parseINI(data []byte) *iniDoc {
	d := &iniDoc{finalNL: len(data) == 0 || data[len(data)-1] == '\n'}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if text == "" {
		return d
	}
	text = strings.TrimSuffix(text, "\n")
	inSection := false
	lastKey := -1
	for _, raw := range strings.Split(text, "\n") {
		l := iniLine{raw: raw}
		trimmed := strings.TrimSpace(raw)
		switch {
		case trimmed == "":
			l.kind = kindBlank
		case trimmed[0] == '#' || trimmed[0] == ';':
			l.kind = kindComment
		case trimmed[0] == '[' && strings.Contains(trimmed, "]") && raw[0] != ' ' && raw[0] != '\t':
			l.kind = kindSection
			l.name = strings.TrimSpace(trimmed[1:strings.Index(trimmed, "]")])
			inSection = true
			lastKey = -1
		case (raw[0] == ' ' || raw[0] == '\t') && lastKey >= 0:
			l.kind = kindCont
			d.lines[lastKey].value += "\n" + trimmed
		case !inSection:
			l.kind = kindOther
		default:
			i := strings.IndexAny(trimmed, "=:")
			if i < 0 {
				l.kind = kindKey
				l.key = strings.ToLower(trimmed)
			} else {
				l.kind = kindKey
				l.key = strings.ToLower(strings.TrimSpace(trimmed[:i]))
				l.value = strings.TrimSpace(trimmed[i+1:])
			}
		}
		d.lines = append(d.lines, l)
		if l.kind == kindKey {
			lastKey = len(d.lines) - 1
		} else if l.kind != kindCont {
			lastKey = -1
		}
	}
	return d
}

func (d *iniDoc) bytes() []byte {
	var b bytes.Buffer
	for i, l := range d.lines {
		b.WriteString(l.raw)
		if i < len(d.lines)-1 || d.finalNL {
			b.WriteByte('\n')
		}
	}
	return b.Bytes()
}

// sectionRange returns the [start,end) line indexes of the body of section
// name (the lines after its header, up to the next header), or ok=false.
func (d *iniDoc) sectionRange(name string) (start, end int, ok bool) {
	for i, l := range d.lines {
		if l.kind == kindSection && l.name == name {
			start = i + 1
			end = len(d.lines)
			for j := start; j < len(d.lines); j++ {
				if d.lines[j].kind == kindSection {
					end = j
					break
				}
			}
			return start, end, true
		}
	}
	return 0, 0, false
}

// sections returns the section names in file order.
func (d *iniDoc) sections() []string {
	var out []string
	for _, l := range d.lines {
		if l.kind == kindSection {
			out = append(out, l.name)
		}
	}
	return out
}

// sectionKeys returns the keys of a section in file order and their values.
func (d *iniDoc) sectionKeys(name string) (order []string, values map[string]string) {
	values = map[string]string{}
	start, end, ok := d.sectionRange(name)
	if !ok {
		return nil, values
	}
	for _, l := range d.lines[start:end] {
		if l.kind != kindKey {
			continue
		}
		if _, seen := values[l.key]; !seen {
			order = append(order, l.key)
		}
		values[l.key] = l.value // like configparser strict=False: last one wins
	}
	return order, values
}

// LoadGlobal reads the [barman] section of a barman.conf file.
func LoadGlobal(path string) (*GlobalConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	doc := parseINI(data)
	order, values := doc.sectionKeys("barman")
	if _, _, ok := doc.sectionRange("barman"); !ok {
		return nil, fmt.Errorf("%s: no [barman] section", path)
	}
	g := &GlobalConfig{Extra: map[string]string{}, Order: order, File: path}
	for k, v := range values {
		switch k {
		case "barman_home":
			g.BarmanHome = v
		case "barman_user":
			g.BarmanUser = v
		case "log_file":
			g.LogFile = v
		case "configuration_files_directory":
			g.ConfigurationFilesDirectory = v
		default:
			g.Extra[k] = v
		}
	}
	return g, nil
}

// LoadServers reads every *.conf file of dir (usually /etc/barman.d) and
// returns the server sections found, sorted by file name then file order. A
// missing directory is not an error: it simply holds no servers.
func LoadServers(dir string) ([]Server, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.conf"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []Server
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		doc := parseINI(data)
		abs, err := filepath.Abs(f)
		if err != nil {
			abs = f
		}
		for _, name := range doc.sections() {
			if name == "barman" {
				continue
			}
			order, values := doc.sectionKeys(name)
			out = append(out, serverFromValues(name, abs, order, values))
		}
	}
	return out, nil
}

func serverFromValues(name, file string, order []string, values map[string]string) Server {
	s := Server{Name: name, File: file, Order: order, Extra: map[string]string{}}
	for k, v := range values {
		switch k {
		case "description":
			s.Description = v
		case "conninfo":
			s.ConnInfo = v
		case "ssh_command":
			s.SSHCommand = v
		case "backup_method":
			s.BackupMethod = v
		case "streaming_conninfo":
			s.StreamingConnInfo = v
		case "streaming_archiver":
			s.StreamingArchiver = v
		case "slot_name":
			s.Slot = v
		case "archiver":
			s.Archiver = v
		case "retention_policy":
			s.RetentionPolicy = v
		case "minimum_redundancy":
			s.MinimumRedundancy = v
		default:
			s.Extra[k] = v
		}
	}
	return s
}

// desired returns the options a Server should have in its file, known keys
// first, then Extra.
func (s Server) values() map[string]string {
	m := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set("description", s.Description)
	set("conninfo", s.ConnInfo)
	set("ssh_command", s.SSHCommand)
	set("backup_method", s.BackupMethod)
	set("streaming_conninfo", s.StreamingConnInfo)
	set("streaming_archiver", s.StreamingArchiver)
	set("slot_name", s.Slot)
	set("archiver", s.Archiver)
	set("retention_policy", s.RetentionPolicy)
	set("minimum_redundancy", s.MinimumRedundancy)
	for k, v := range s.Extra {
		m[strings.ToLower(k)] = v
	}
	return m
}

var serverNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// ValidName reports whether name can be used as a Barman server name and as a
// file name in barman.d.
func ValidName(name string) bool {
	return name != "barman" && serverNameRE.MatchString(name)
}

// SaveNewServer creates dir/<name>.conf for a server that has no file yet. It
// refuses to overwrite an existing file.
func SaveNewServer(dir string, server Server) error {
	if !ValidName(server.Name) {
		return fmt.Errorf("invalid server name %q: use letters, digits, '-', '_' and '.'", server.Name)
	}
	server.File = filepath.Join(dir, server.Name+".conf")
	if _, err := os.Stat(server.File); err == nil {
		return fmt.Errorf("%s already exists", server.File)
	}
	return SaveServer(server)
}

// SaveServer writes server back to server.File. If the file exists, only the
// section named server.Name is changed and only where a value differs:
// comments, blank lines, key order, unknown keys and the other sections of the
// file are kept as they are. Keys that are no longer set are removed, new keys
// are appended to the section. If the file does not exist it is created.
// The write is atomic (temporary file + rename) and the mode is 0640.
func SaveServer(server Server) error {
	if server.File == "" {
		return errors.New("server has no file: use SaveNewServer for a new server")
	}
	if !ValidName(server.Name) {
		return fmt.Errorf("invalid server name %q", server.Name)
	}
	var doc *iniDoc
	data, err := os.ReadFile(server.File)
	switch {
	case err == nil:
		doc = parseINI(data)
	case errors.Is(err, os.ErrNotExist):
		doc = &iniDoc{finalNL: true}
	default:
		return err
	}
	applySection(doc, server)
	return writeAtomic(server.File, doc.bytes(), 0o640)
}

func applySection(doc *iniDoc, s Server) {
	want := s.values()
	start, end, ok := doc.sectionRange(s.Name)
	if !ok {
		if len(doc.lines) > 0 && doc.lines[len(doc.lines)-1].kind != kindBlank {
			doc.lines = append(doc.lines, iniLine{kind: kindBlank})
		}
		doc.lines = append(doc.lines, iniLine{raw: "[" + s.Name + "]", kind: kindSection, name: s.Name})
		start, end = len(doc.lines), len(doc.lines)
		doc.finalNL = true
	}

	done := map[string]bool{}
	var body []iniLine
	skipCont := false
	for _, l := range doc.lines[start:end] {
		switch l.kind {
		case kindKey:
			skipCont = false
			v, keep := want[l.key]
			switch {
			case !keep:
				skipCont = true // drop the key and its continuation lines
			case v == l.value:
				done[l.key] = true
				body = append(body, l)
			default:
				done[l.key] = true
				skipCont = true
				body = append(body, iniLine{raw: l.key + " = " + v, kind: kindKey, key: l.key, value: v})
			}
		case kindCont:
			if !skipCont {
				body = append(body, l)
			}
		default:
			skipCont = false
			body = append(body, l)
		}
	}

	// Append missing keys: file order hint first, then known keys, then the
	// rest sorted, so a new file reads the way a person would write it.
	var missing []string
	seen := map[string]bool{}
	add := func(k string) {
		if _, w := want[k]; w && !done[k] && !seen[k] {
			seen[k] = true
			missing = append(missing, k)
		}
	}
	for _, k := range s.Order {
		add(k)
	}
	for _, k := range knownKeys {
		add(k)
	}
	var rest []string
	for k := range want {
		if !done[k] && !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		add(k)
	}
	// Insert before the trailing blank/comment lines of the section so that
	// a blank line separating two sections stays a separator.
	insertAt := len(body)
	for insertAt > 0 && body[insertAt-1].kind == kindBlank {
		insertAt--
	}
	var added []iniLine
	for _, k := range missing {
		added = append(added, iniLine{raw: k + " = " + want[k], kind: kindKey, key: k, value: want[k]})
	}
	body = append(body[:insertAt], append(added, body[insertAt:]...)...)

	out := make([]iniLine, 0, len(doc.lines)+len(added))
	out = append(out, doc.lines[:start]...)
	out = append(out, body...)
	out = append(out, doc.lines[end:]...)
	doc.lines = out
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	w := bufio.NewWriter(tmp)
	if _, err := w.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := w.Flush(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "true", "yes", "1":
		return true
	}
	return false
}

// Validate returns the problems found in a server definition, as sentences a
// person can act on. An empty result means the definition is complete for its
// backup method. Barman's default method, when backup_method is not set, is
// rsync.
func Validate(server Server) []string {
	var p []string
	if !ValidName(server.Name) {
		p = append(p, fmt.Sprintf("The server name %q is not valid: use letters, digits, '-', '_' and '.', starting with a letter or a digit.", server.Name))
	}
	method := strings.ToLower(strings.TrimSpace(server.BackupMethod))
	if method == "" {
		method = "rsync"
	}
	switch method {
	case "postgres":
		if strings.TrimSpace(server.ConnInfo) == "" {
			p = append(p, "backup_method = postgres needs conninfo, the connection string Barman uses to query PostgreSQL.")
		}
		if strings.TrimSpace(server.StreamingConnInfo) == "" {
			p = append(p, "backup_method = postgres needs streaming_conninfo, the replication connection used by pg_basebackup and pg_receivewal.")
		}
		if !isOn(server.StreamingArchiver) {
			p = append(p, "backup_method = postgres needs streaming_archiver = on so that WAL files are streamed.")
		}
		if strings.TrimSpace(server.Slot) == "" {
			p = append(p, "backup_method = postgres needs slot_name, the replication slot that keeps WAL files until Barman has them.")
		}
	case "rsync":
		if strings.TrimSpace(server.SSHCommand) == "" {
			p = append(p, "backup_method = rsync needs ssh_command, for example \"ssh postgres@db1\".")
		}
		if strings.TrimSpace(server.ConnInfo) == "" {
			p = append(p, "backup_method = rsync needs conninfo, the connection string Barman uses to query PostgreSQL.")
		}
		if !isOn(server.Archiver) {
			p = append(p, "backup_method = rsync needs archiver = on so that PostgreSQL ships its WAL files with archive_command.")
		}
	default:
		p = append(p, fmt.Sprintf("backup_method %q is not supported: use postgres (streaming) or rsync.", server.BackupMethod))
	}
	return p
}
