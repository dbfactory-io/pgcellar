// pgcellar: a control room for PostgreSQL backups, built on Barman.
//
// Subcommands:
//
//	pgcellar serve --config /etc/pgcellar/pgcellar.yaml   run the web UI and API
//	pgcellar check --config ...                            validate the configuration and list the servers
//	pgcellar version
//
// The real work lives in internal/. This file only parses the command line.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/stranix79/pgcellar/internal/app"
	"github.com/stranix79/pgcellar/internal/barman"
	"github.com/stranix79/pgcellar/internal/config"
	"github.com/stranix79/pgcellar/internal/web"
)

// Version is set at build time by -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		usage(stderr)
		return 2
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "pgcellar", Version)
		return 0
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "check":
		return check(args[1:], stdout, stderr)
	case "help", "--help", "-h":
		usage(stdout)
		return 0
	}
	usage(stderr)
	return 2
}

func usage(w io.Writer) {
	fmt.Fprintln(w, `usage:
  pgcellar serve --config FILE [--listen ADDR] [--dev] [--cron-interval 30s]
  pgcellar check --config FILE [--barman-dir DIR]
  pgcellar version`)
}

func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/pgcellar/pgcellar.yaml", "path of pgcellar.yaml")
	listen := fs.String("listen", "", "address to listen on (overrides listen: in pgcellar.yaml)")
	dev := fs.Bool("dev", false, "development mode: fake Barman, temporary data, user admin/admin")
	cronEvery := fs.String("cron-interval", "", "how often to run 'barman cron', for example 30s; 0 disables it (default: barman.cron_interval in pgcellar.yaml)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	var cronOverride time.Duration
	if *cronEvery != "" {
		d, err := time.ParseDuration(*cronEvery)
		if err != nil && *cronEvery == "0" {
			d, err = 0, nil
		}
		if err != nil || d < 0 {
			fmt.Fprintf(stderr, "pgcellar: invalid --cron-interval %q\n", *cronEvery)
			return 2
		}
		cronOverride = d
	}

	a, err := app.Load(*cfgPath, *dev)
	if err != nil {
		fmt.Fprintln(stderr, "pgcellar:", err)
		return 1
	}
	a.Version = Version
	if *cronEvery != "" {
		a.CronInterval = cronOverride
	}
	addr := *listen
	if addr == "" {
		addr = a.Config().Listen
	}

	srv, err := web.New(a)
	if err != nil {
		fmt.Fprintln(stderr, "pgcellar:", err)
		_ = a.Close()
		return 1
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(stderr, "pgcellar: cannot listen:", err)
		_ = a.Close()
		return 1
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.Start(ctx)

	fmt.Fprintf(stdout, "pgcellar %s listening on %s\n", Version, displayURL(ln.Addr().String()))
	if *dev {
		fmt.Fprintf(stdout, "dev mode: fake Barman, data in %s, sign in as admin / admin\n", a.TempDir)
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.Serve(ln) }()

	code := 0
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "pgcellar:", err)
			code = 1
		}
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = httpSrv.Shutdown(shutdown)
		cancel()
	}
	stop()
	if err := a.Close(); err != nil {
		fmt.Fprintln(stderr, "pgcellar:", err)
	}
	return code
}

func displayURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr
	}
	if host == "" || host == "::" || host == "0.0.0.0" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func check(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/pgcellar/pgcellar.yaml", "path of pgcellar.yaml")
	dir := fs.String("barman-dir", "", "read the Barman server files from this directory instead of barman.config_dir")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "pgcellar: cannot read %s: %v\n", *cfgPath, err)
		return 1
	}
	bad := 0
	fmt.Fprintf(stdout, "configuration %s\n", *cfgPath)
	if problems := config.Validate(cfg); len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintf(stdout, "  PROBLEM  %s\n", p)
			bad++
		}
	} else {
		fmt.Fprintln(stdout, "  ok")
	}

	barmanDir := *dir
	if barmanDir == "" {
		barmanDir = cfg.Barman.ConfigDir
		if _, err := os.Stat(barmanDir); err != nil {
			// A copy of the example config points to /etc/barman.d, which only exists
			// on a Barman host: use the example files next to it.
			if alt := filepath.Join(filepath.Dir(*cfgPath), "barman.d.example"); isDir(alt) {
				fmt.Fprintf(stdout, "note: %s not found, reading %s instead\n", barmanDir, alt)
				barmanDir = alt
			}
		}
	}
	servers, err := barman.LoadServers(barmanDir)
	if err != nil {
		fmt.Fprintf(stdout, "  PROBLEM  cannot read the Barman directory %s: %v\n", barmanDir, err)
		return 1
	}
	fmt.Fprintf(stdout, "\nservers in %s: %d\n", barmanDir, len(servers))
	seen := map[string]bool{}
	for _, s := range servers {
		seen[s.Name] = true
		method := s.BackupMethod
		if method == "" {
			method = "rsync (default)"
		}
		probs := barman.Validate(s)
		status := "ok"
		if len(probs) > 0 {
			status = "PROBLEM"
			bad++
		}
		fmt.Fprintf(stdout, "  %-8s %-20s %-16s %s\n", status, s.Name, method, filepath.Base(s.File))
		for _, p := range probs {
			fmt.Fprintf(stdout, "           - %s\n", p)
		}
	}
	for _, s := range cfg.Servers {
		if !seen[s.Name] {
			fmt.Fprintf(stdout, "  PROBLEM  %-20s listed in pgcellar.yaml but has no section in %s\n", s.Name, barmanDir)
			bad++
		}
	}
	if bad > 0 {
		fmt.Fprintf(stdout, "\n%d problem(s) found\n", bad)
		return 1
	}
	fmt.Fprintln(stdout, "\nall good")
	return 0
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
