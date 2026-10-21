package app

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/barman"
	"github.com/dbfactory-io/pgcellar/internal/config"
)

// journalRunner puts every Barman command of the drill in the journal.
type journalRunner struct {
	r     *drillRun
	inner barman.Runner
}

func (j *journalRunner) Run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	j.r.logf("$ barman %s", strings.Join(args, " "))
	out, errOut, err := j.inner.Run(ctx, args...)
	if s := clip(out, 800); s != "" {
		j.r.logf("  %s", s)
	}
	if s := clip(errOut, 800); s != "" {
		j.r.logf("  stderr: %s", s)
	}
	return out, errOut, err
}

// drillDest is where the copy of a drill is restored:
// <root>/<server>/drill-<timestamp>, one level below the server directory like
// every restore, so the same path checks apply.
func drillDest(root, server string, at time.Time) (string, error) {
	if !barman.ValidName(server) {
		return "", fmt.Errorf("refusing to build a restore path for %q", server)
	}
	base := filepath.Join(root, server, "drill-"+at.Format("20060102T150405"))
	dest := base
	for i := 2; ; i++ {
		if _, err := os.Lstat(dest); errors.Is(err, os.ErrNotExist) {
			break
		}
		dest = base + "-" + strconv.Itoa(i)
	}
	if err := underRecoveries(root, dest); err != nil && !errors.Is(err, errNothingThere) {
		return "", err
	}
	return dest, nil
}

// tool is the path of a PostgreSQL program for this drill.
func (r *drillRun) tool(name string) string {
	t := r.a.DrillTools
	switch name {
	case "pg_ctl":
		if t.PgCtl != "" {
			return t.PgCtl
		}
	case "pg_isready":
		if t.PgIsReady != "" {
			return t.PgIsReady
		}
	case "pg_checksums":
		if t.PgChecksums != "" {
			return t.PgChecksums
		}
	}
	return filepath.Join(r.bin, name)
}

func (a *App) drillPoll() time.Duration {
	if a.DrillPollInterval > 0 {
		return a.DrillPollInterval
	}
	return 2 * time.Second
}

// freePort finds a port of 54400-54499 nobody listens on.
func freePort() (int, error) {
	n := drillPortMax - drillPortMin + 1
	start := rand.IntN(n)
	for i := 0; i < n; i++ {
		p := drillPortMin + (start+i)%n
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(p))
		if err == nil {
			_ = l.Close()
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port between %d and %d on 127.0.0.1", drillPortMin, drillPortMax)
}

var restoreCommandRe = regexp.MustCompile(`(?m)^\s*restore_command\s*=.*$`)

// chownBarman gives a file to the Barman user when pgCellar runs as root and
// drives Barman as that user, like prepareRecoveryDir does for directories.
func chownBarman(cfg *config.Config, paths ...string) {
	if os.Geteuid() != 0 || cfg.Barman.User == "" {
		return
	}
	u, err := user.Lookup(cfg.Barman.User)
	if err != nil {
		return
	}
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	for _, p := range paths {
		_ = os.Chown(p, uid, gid)
	}
}

// configure writes the settings of the throwaway copy after what Barman wrote
// (the last value of a parameter wins in postgresql.auto.conf), so the
// restore_command that Barman prepared stays.
func (r *drillRun) configure() (string, error) {
	port, err := freePort()
	if err != nil {
		return "", err
	}
	r.port = port
	r.det.Port = port
	r.sock = r.dest
	if len(r.sock)+len("/.s.PGSQL.54400") > 100 { // a Unix socket path is limited to about 107 bytes
		d, err := os.MkdirTemp("", "pgc-drill-")
		if err != nil {
			return "", err
		}
		_ = os.Chmod(d, 0o750)
		chownBarman(r.cfg, d)
		r.sock = d
		r.extraDirs = append(r.extraDirs, d)
	}
	autoPath := filepath.Join(r.dest, "postgresql.auto.conf")
	existing, err := os.ReadFile(autoPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("cannot read postgresql.auto.conf: %w", err)
	}
	note := "settings written"
	if m := restoreCommandRe.FindAll(existing, -1); len(m) > 0 {
		r.det.RestoreCmd = strings.TrimSpace(string(m[len(m)-1]))
		r.logf("  Barman's %s is kept", r.det.RestoreCmd)
	} else if hasWALSegments(filepath.Join(r.dest, "pg_wal")) {
		r.logf("  Barman placed the required WAL in pg_wal; no restore_command needed")
	} else {
		note += "; Barman wrote no restore_command and pg_wal holds no segment, recovery can only use the WAL inside the backup"
		r.logf("  warning: no restore_command in postgresql.auto.conf and no WAL segment in pg_wal")
	}
	hba := filepath.Join(r.dest, "pg_hba_drill.conf")
	if err := os.WriteFile(hba, []byte("# pgCellar restore drill: the Unix socket of this directory only\nlocal all all trust\nhost all all 0.0.0.0/0 reject\nhost all all ::/0 reject\n"), 0o640); err != nil {
		return "", err
	}
	var b strings.Builder
	b.Write(existing)
	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "\n# --- pgCellar restore drill: settings of the throwaway copy ---\n")
	fmt.Fprintf(&b, "port = %d\n", port)
	b.WriteString("listen_addresses = '127.0.0.1'\n")
	fmt.Fprintf(&b, "unix_socket_directories = '%s'\n", strings.ReplaceAll(r.sock, "'", "''"))
	fmt.Fprintf(&b, "hba_file = '%s'\n", strings.ReplaceAll(hba, "'", "''"))
	b.WriteString("archive_mode = off\n")
	b.WriteString("max_connections = 10\n")
	b.WriteString("shared_buffers = 64MB\n")
	// With max_connections lower than on the source, a standby that accepts
	// connections during recovery refuses to start: the copy does not accept
	// any until the recovery is over.
	b.WriteString("hot_standby = off\n")
	b.WriteString("max_wal_senders = 0\n")
	b.WriteString("autovacuum = off\n")
	b.WriteString("logging_collector = off\n")
	b.WriteString("ssl = off\n")
	// Session locale parameters: the messages of the copy do not depend on
	// which locales this image has (the cluster's own collation does, see start).
	b.WriteString("lc_messages = 'C'\nlc_monetary = 'C'\nlc_numeric = 'C'\nlc_time = 'C'\n")
	b.WriteString("jit = off\n") // no JIT compile for a few queries
	// An extension library missing from this image must not stop the copy.
	b.WriteString("shared_preload_libraries = ''\n")
	b.WriteString("synchronous_standby_names = ''\n")
	if r.in.Target != TargetLatest {
		b.WriteString("recovery_target_action = 'promote'\n")
	}
	if err := os.WriteFile(autoPath, []byte(b.String()), 0o600); err != nil {
		return "", fmt.Errorf("cannot write postgresql.auto.conf: %w", err)
	}
	chownBarman(r.cfg, autoPath, hba)
	return fmt.Sprintf("port %d, socket %s; %s", port, filepath.Base(r.sock), note), nil
}

func (r *drillRun) logTail() string { return r.logLines(25) }

func (r *drillRun) logLines(n int) string {
	raw, err := os.ReadFile(filepath.Join(r.dest, "drill.log"))
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// hasWALSegments reports whether dir holds a WAL segment file (24 hex digits).
func hasWALSegments(dir string) bool {
	es, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range es {
		if walNameRe.MatchString(e.Name()) {
			return true
		}
	}
	return false
}

var (
	walNameRe    = regexp.MustCompile(`^[0-9A-F]{24}$`)
	localeErrRes = []*regexp.Regexp{
		regexp.MustCompile(`LC_(?:COLLATE|CTYPE) "([^"]+)"`),
		regexp.MustCompile(`invalid value for parameter "lc_[a-z]+": "([^"]+)"`),
	}
)

// localeProblem turns a start failure caused by a locale this image lacks into
// a sentence that says what to do. "" when the log says nothing of the kind.
func localeProblem(log string) string {
	for _, re := range localeErrRes {
		if m := re.FindStringSubmatch(log); m != nil {
			name := m[1]
			want := name
			if i := strings.LastIndex(want, ".utf8"); i > 0 && i == len(want)-5 {
				want = want[:i] + ".UTF-8"
			}
			return fmt.Sprintf("The cluster uses locale %s, which this image does not have. Rebuild with PG_LOCALES including %s (see deploy/Dockerfile).", name, want)
		}
	}
	if strings.Contains(strings.ToLower(log), "locale") {
		return "The cluster needs a locale this image does not have (see the PostgreSQL log below). Rebuild with PG_LOCALES including it (see deploy/Dockerfile)."
	}
	return ""
}

var connUserRe = regexp.MustCompile(`(?:^|\s)user\s*=\s*('(?:[^'\\]|\\.)*'|\S+)`)

// pgUsers lists the roles to try on the copy: the one set in the server's
// conninfo, then postgres. The copy trusts every local connection, so only the
// name matters, and the superuser of the source cluster is not known.
func (r *drillRun) pgUsers() []string {
	var out []string
	add := func(u string) {
		u = strings.Trim(strings.TrimSpace(u), "'")
		if u == "" {
			return
		}
		for _, o := range out {
			if o == u {
				return
			}
		}
		out = append(out, u)
	}
	if files, err := barman.LoadServers(r.a.barmanConfigDir()); err == nil {
		if s := findFile(files, r.server); s != nil {
			for _, ci := range []string{s.ConnInfo, s.StreamingConnInfo} {
				if m := connUserRe.FindStringSubmatch(ci); m != nil {
					add(m[1])
				}
			}
		}
	}
	add("postgres")
	return out
}

func (r *drillRun) psqlProgram() string {
	if p := r.a.SetupTools.Psql; p != "" {
		return p
	}
	return "psql"
}

// psql runs one query on the copy and returns the trimmed answer. The first
// call finds the role that exists.
func (r *drillRun) psql(db, sql string) (string, error) {
	users := []string{r.pgUser}
	if r.pgUser == "" {
		users = r.pgUsers()
	}
	var last error
	for _, u := range users {
		out, err := r.exec(r.ctx, r.psqlProgram(), "-X", "-At", "-F", "\t", "-v", "ON_ERROR_STOP=1",
			"-h", r.sock, "-p", strconv.Itoa(r.port), "-U", u, "-d", db, "-c", sql)
		if err == nil {
			if r.pgUser == "" {
				r.pgUser, r.det.PGUser = u, u
			}
			return strings.TrimSpace(string(out)), nil
		}
		last = err
		if r.pgUser != "" || !strings.Contains(errText(err), "does not exist") {
			break
		}
	}
	return "", last
}

// waitRecovery waits until the copy accepts connections (pg_isready), which
// with hot_standby off means that the recovery is over and the server was
// promoted, then asks it.
func (r *drillRun) waitRecovery() (string, error) {
	limit := r.cfg.Drills.RecoveryTimeout
	deadline := time.Now().Add(limit)
	poll := r.a.drillPoll()
	wait := func() error {
		select {
		case <-r.ctx.Done():
			return r.ctx.Err()
		case <-time.After(poll):
			return nil
		}
	}
	timeout := fmt.Errorf("the copy did not finish its recovery within drills.recovery_timeout (%s)", limit)
	polls := 0
	for {
		polls++
		_, err := r.execQuiet(r.ctx, r.tool("pg_isready"), "-h", r.sock, "-p", strconv.Itoa(r.port))
		if err == nil {
			break
		}
		if ec := exitCodeOf(err); ec != 1 && ec != 2 {
			return "", err
		} else if ec == 2 {
			if _, serr := r.execQuiet(r.ctx, r.tool("pg_ctl"), "-D", r.dest, "status"); serr != nil {
				r.logf("  the server is not running any more")
				return "", errors.New("the server stopped while recovering: " + firstNonEmpty(r.logTail(), "see the PostgreSQL log"))
			}
		}
		if time.Now().After(deadline) {
			return "", timeout
		}
		if err := wait(); err != nil {
			return "", err
		}
	}
	r.logf("  pg_isready: accepting connections after %d look(s)", polls)
	for {
		ans, err := r.psql("template1", "select pg_is_in_recovery()")
		if err != nil {
			return "", err
		}
		if ans == "f" {
			return "recovery finished, the copy is a normal server", nil
		}
		if time.Now().After(deadline) {
			return "", timeout
		}
		if err := wait(); err != nil {
			return "", err
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// execQuiet runs a program without writing it to the journal (polling).
func (r *drillRun) execQuiet(ctx context.Context, binary string, args ...string) ([]byte, error) {
	out, _, err := r.a.Tools.RunBinary(barman.WithTarget(ctx, r.server), binary, args...)
	return out, err
}

func firstLineWith(out []byte, sub string) string {
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, sub) {
			return strings.Join(strings.Fields(l), " ")
		}
	}
	return "done"
}

// check asks the copy what it holds.
func (r *drillRun) check() (string, error) {
	var err error
	if r.det.PGVersion, err = r.psql("template1", "select version()"); err != nil {
		return "", err
	}
	// After a promotion pg_last_wal_replay_lsn keeps the last replayed position
	// on recent versions; the current position covers the others.
	if r.det.LSN, err = r.psql("template1", "select coalesce(pg_last_wal_replay_lsn(), pg_current_wal_lsn())"); err != nil {
		return "", err
	}
	cs, err := r.psql("template1", "show data_checksums")
	if err != nil {
		return "", err
	}
	if cs == "on" {
		r.det.Checksums = "enabled"
	} else {
		r.det.Checksums = "not enabled"
	}
	list, err := r.psql("template1", "select datname, pg_database_size(datname) from pg_database where datallowconn and not datistemplate order by datname")
	if err != nil {
		return "", err
	}
	r.det.Databases = nil
	totalTables := 0
	for _, line := range strings.Split(list, "\n") {
		f := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(f) != 2 || f[0] == "" {
			continue
		}
		size, _ := strconv.ParseInt(f[1], 10, 64)
		db := DrillDatabase{Name: f[0], SizeBytes: size}
		n, err := r.psql(db.Name, "select count(*) from pg_stat_user_tables")
		if err != nil {
			return "", fmt.Errorf("database %s: %w", db.Name, err)
		}
		if db.Tables, err = strconv.Atoi(n); err != nil {
			return "", fmt.Errorf("database %s: the table count %q is not a number", db.Name, n)
		}
		totalTables += db.Tables
		if r.cfg.Drills.VerifyTables && db.Tables > 0 {
			names, err := r.psql(db.Name, "select format('%I.%I', schemaname, relname) from pg_stat_user_tables order by 1")
			if err != nil {
				return "", fmt.Errorf("database %s: %w", db.Name, err)
			}
			var rows int64
			for _, t := range strings.Split(names, "\n") {
				if strings.TrimSpace(t) == "" {
					continue
				}
				c, err := r.psql(db.Name, "select count(*) from "+t)
				if err != nil {
					return "", fmt.Errorf("database %s, table %s: %w", db.Name, t, err)
				}
				v, _ := strconv.ParseInt(c, 10, 64)
				rows += v
			}
			db.Rows = &rows
		}
		r.det.Databases = append(r.det.Databases, db)
	}
	if len(r.det.Databases) == 0 {
		return "", errors.New("the copy lists no database")
	}
	r.det.DatabasesN, r.det.TablesN = len(r.det.Databases), totalTables
	return fmt.Sprintf("%s, LSN %s, %s", plural(len(r.det.Databases), "database"), r.det.LSN, plural(totalTables, "table")), nil
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// HumanBytes writes a size for people: "12.0 MiB".
func HumanBytes(n int64) string {
	if n <= 0 {
		return "0 B"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanBytes(n int64) string { return HumanBytes(n) }
