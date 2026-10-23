package barman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// CLIRunner runs the Barman command line. It is the only place in pgCellar
// that starts Barman: every command is recorded through Audit with its exact
// command line, exit code and duration.
type CLIRunner struct {
	Binary string // "barman" when empty
	User   string // run as this user through sudo when it differs from the current user
	Audit  Audit  // optional
	// OnlyUnder, when set, refuses to run any program that does not resolve to a
	// file under this directory. The public demo uses it so that it can never
	// start the real Barman or the real psql.
	OnlyUnder string
}

// RunError is returned when Barman ran and exited with a non-zero status, or
// could not be started (ExitCode -1).
type RunError struct {
	Command  string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *RunError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg != "" {
		return fmt.Sprintf("%s: exit %d: %s", e.Command, e.ExitCode, msg)
	}
	return fmt.Sprintf("%s: exit %d: %v", e.Command, e.ExitCode, e.Err)
}

func (e *RunError) Unwrap() error { return e.Err }

type actorKey struct{}

// WithActor returns a context that tags the audit entries of the commands run
// with it (a user name or an API key name).
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFrom returns the actor set by WithActor, or "".
func ActorFrom(ctx context.Context) string {
	a, _ := ctx.Value(actorKey{}).(string)
	return a
}

type targetKey struct{}

// WithTarget returns a context that names the server the commands run with it
// are about; it is recorded in the audit entries of tools other than Barman,
// whose arguments do not end with the server name.
func WithTarget(ctx context.Context, server string) context.Context {
	return context.WithValue(ctx, targetKey{}, server)
}

// TargetFrom returns the server set by WithTarget, or "".
func TargetFrom(ctx context.Context) string {
	t, _ := ctx.Value(targetKey{}).(string)
	return t
}

// currentUser is a variable so tests can pretend to be someone else.
var currentUser = func() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return os.Getenv("USER")
}

// argv builds the full command line for the given Barman arguments.
func (r CLIRunner) argv(args []string) []string {
	bin := r.Binary
	if bin == "" {
		bin = "barman"
	}
	return r.argvFor(bin, args)
}

// argvFor builds the full command line for any binary, with the same sudo rule.
func (r CLIRunner) argvFor(bin string, args []string) []string {
	var out []string
	if r.User != "" && r.User != currentUser() {
		out = append(out, "sudo", "-u", r.User)
	}
	out = append(out, bin)
	return append(out, args...)
}

// argvEnv is argvFor; through sudo, the names of env are kept with
// --preserve-env (sudo clears the environment otherwise).
func (r CLIRunner) argvEnv(bin string, args, env []string) []string {
	argv := r.argvFor(bin, args)
	if len(env) == 0 || argv[0] != "sudo" {
		return argv
	}
	names := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	return append([]string{"sudo", "--preserve-env=" + strings.Join(names, ",")}, argv[1:]...)
}

// Run executes `barman <args...>`. stdout and stderr are returned even when
// the command fails, so callers can still parse the JSON Barman prints before
// exiting non-zero (barman check does that). The error is a *RunError for a
// non-zero exit, or the context error when the context ended first.
func (r CLIRunner) Run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	bin := r.Binary
	if bin == "" {
		bin = "barman"
	}
	return r.RunBinary(ctx, bin, args...)
}

// RunBinary executes `<binary> <args...>` through the same path as Run: same
// user switch, same audit entry. It exists for the few read-only tools pgCellar
// needs next to Barman (psql for the server setup, pg_basebackup --version);
// nothing else in pgCellar starts a process.
func (r CLIRunner) RunBinary(ctx context.Context, binary string, args ...string) (stdout, stderr []byte, err error) {
	return r.RunBinaryEnv(ctx, nil, binary, args...)
}

// RunBinaryEnv is RunBinary with extra environment variables ("NAME=value")
// for the program: restic reads its S3 credentials there. The values never
// reach the command line, the audit log nor the journal; through sudo the
// names are passed with --preserve-env.
func (r CLIRunner) RunBinaryEnv(ctx context.Context, env []string, binary string, args ...string) (stdout, stderr []byte, err error) {
	argv := r.argvEnv(binary, args, env)
	command := strings.Join(argv, " ")
	if r.OnlyUnder != "" {
		if err := underDir(binary, r.OnlyUnder); err != nil {
			return nil, nil, &RunError{Command: command, ExitCode: -1, Stderr: err.Error(), Err: err}
		}
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	dur := time.Since(start)

	code := 0
	if runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) && ee.ExitCode() >= 0 {
			code = ee.ExitCode()
		} else {
			code = -1
		}
	}
	if r.Audit != nil {
		r.Audit(AuditEntry{
			When:     start,
			Args:     append([]string(nil), args...),
			Binary:   binary,
			Target:   TargetFrom(ctx),
			Command:  command,
			ExitCode: code,
			Duration: dur,
			Actor:    ActorFrom(ctx),
		})
	}
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return out.Bytes(), errb.Bytes(), ctxErr
		}
		return out.Bytes(), errb.Bytes(), &RunError{Command: command, ExitCode: code, Stderr: errb.String(), Err: runErr}
	}
	return out.Bytes(), errb.Bytes(), nil
}

// underDir checks that the program name resolves (PATH included) to a file
// inside dir, symbolic links followed.
func underDir(binary, dir string) error {
	path, err := exec.LookPath(binary)
	if err != nil {
		return fmt.Errorf("refusing to run %s: %w", binary, err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	// The link itself (barman -> fake-barman) lives in dir, its target too.
	if filepath.Dir(real) != root {
		return fmt.Errorf("refusing to run %s: it is %s, outside %s", binary, real, root)
	}
	return nil
}

// CheckOnlyUnder reports an error unless every name resolves under dir.
func CheckOnlyUnder(dir string, names ...string) error {
	for _, n := range names {
		if err := underDir(n, dir); err != nil {
			return err
		}
	}
	return nil
}
