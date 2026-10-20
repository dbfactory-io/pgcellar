package barman

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
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
	var out []string
	if r.User != "" && r.User != currentUser() {
		out = append(out, "sudo", "-u", r.User)
	}
	out = append(out, bin)
	return append(out, args...)
}

// Run executes `barman <args...>`. stdout and stderr are returned even when
// the command fails, so callers can still parse the JSON Barman prints before
// exiting non-zero (barman check does that). The error is a *RunError for a
// non-zero exit, or the context error when the context ended first.
func (r CLIRunner) Run(ctx context.Context, args ...string) (stdout, stderr []byte, err error) {
	argv := r.argv(args)
	command := strings.Join(argv, " ")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
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
