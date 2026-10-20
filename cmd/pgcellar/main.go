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
	"fmt"
	"os"
)

// Version is set at build time by -ldflags "-X main.Version=...".
var Version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "version":
		fmt.Println("pgcellar", Version)
	case "serve", "check":
		fmt.Fprintf(os.Stderr, "pgcellar %s: not implemented yet\n", os.Args[1])
		os.Exit(1)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pgcellar <serve|check|version> [--config FILE]")
}
