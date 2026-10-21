package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"github.com/dbfactory-io/pgcellar/internal/config"
)

// apikey implements `pgcellar apikey create|list`. Keys live in pgcellar.yaml
// as a SHA-256 digest; a running server picks the change up by itself.
func apikey(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		_, _ = fmt.Fprintln(stderr, "usage: pgcellar apikey create --config FILE --name NAME --role viewer|operator|admin | pgcellar apikey list --config FILE")
		return 2
	}
	fs := flag.NewFlagSet("apikey "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := fs.String("config", "/etc/pgcellar/pgcellar.yaml", "path of pgcellar.yaml")
	name := fs.String("name", "", "name of the key (create)")
	role := fs.String("role", "", "viewer, operator or admin (create)")
	switch args[0] {
	case "create", "list":
	default:
		_, _ = fmt.Fprintf(stderr, "pgcellar: unknown apikey command %q\n", args[0])
		return 2
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: cannot read %s: %v\n", *cfgPath, err)
		return 1
	}
	if args[0] == "list" {
		for _, k := range cfg.APIKeys {
			_, _ = fmt.Fprintf(stdout, "%s\t%s\n", k.Name, k.Role)
		}
		return 0
	}

	if *name == "" || *role == "" {
		_, _ = fmt.Fprintln(stderr, "pgcellar: --name and --role are required")
		return 2
	}
	clear, hash := config.NewAPIKey()
	cfg.APIKeys = append(cfg.APIKeys, config.APIKey{Name: *name, Role: *role, KeyHash: hash})
	if problems := config.Validate(cfg); len(problems) > 0 {
		for _, p := range problems {
			_, _ = fmt.Fprintln(stderr, "pgcellar:", p)
		}
		return 1
	}
	if err := config.Save(*cfgPath, cfg); err != nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: cannot write %s: %v\n", *cfgPath, err)
		return 1
	}
	if _, err := config.Commit(filepath.Dir(*cfgPath), fmt.Sprintf("Create API key %s (%s)", *name, *role), "cli", "cli@pgcellar"); err != nil {
		_, _ = fmt.Fprintf(stderr, "pgcellar: warning: key saved but not recorded in the history: %v\n", err)
	}
	_, _ = fmt.Fprintln(stdout, clear)
	return 0
}
