package barman

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// copyKeyDropped reports whether an option must not follow a server into the
// configuration that reads a copy of its directory: paths that point to the
// live directories, and hooks, which would run scripts of the live host.
func copyKeyDropped(k string) bool {
	k = strings.ToLower(k)
	switch k {
	case "barman_home", "barman_user", "configuration_files_directory", "log_file", "barman_lock_directory":
		return true
	}
	return strings.HasSuffix(k, "_directory") || strings.Contains(k, "_script") || strings.Contains(k, "_hook")
}

// WriteCopyConfig writes into dir a barman.conf and a barman.d/<name>.conf
// with which Barman reads a copy of a server's directory found at
// <home>/<name> (barman -c <dir>/barman.conf ...), for example a copy fetched
// back from the off-site vault. The options of the global section and of the
// server section are kept, except the paths that point to the live
// directories and the hooks; server may be nil (no barman.d file was copied),
// then a minimal section is written. It returns the path of barman.conf.
func WriteCopyConfig(dir, home, user string, global *GlobalConfig, server *Server, name string) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("invalid server name %q", name)
	}
	confDir := filepath.Join(dir, "barman.d")
	locks := filepath.Join(dir, "locks")
	for _, d := range []string{confDir, locks} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return "", err
		}
	}
	var g strings.Builder
	g.WriteString("# Written by pgCellar to read a copy of the Barman directory of " + name + ".\n[barman]\n")
	fmt.Fprintf(&g, "barman_home = %s\n", home)
	if user != "" {
		fmt.Fprintf(&g, "barman_user = %s\n", user)
	}
	fmt.Fprintf(&g, "configuration_files_directory = %s\n", confDir)
	fmt.Fprintf(&g, "barman_lock_directory = %s\n", locks)
	fmt.Fprintf(&g, "log_file = %s\n", filepath.Join(dir, "barman.log"))
	if global != nil {
		keys := make([]string, 0, len(global.Extra))
		for k := range global.Extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !copyKeyDropped(k) {
				fmt.Fprintf(&g, "%s = %s\n", k, global.Extra[k])
			}
		}
	}
	var s strings.Builder
	fmt.Fprintf(&s, "[%s]\n", name)
	if server != nil {
		vals := server.values()
		order := append([]string(nil), server.Order...)
		seen := map[string]bool{}
		for _, k := range order {
			seen[strings.ToLower(k)] = true
		}
		var rest []string
		for k := range vals {
			if !seen[k] {
				rest = append(rest, k)
			}
		}
		sort.Strings(rest)
		for _, k := range append(order, rest...) {
			k = strings.ToLower(k)
			if v, ok := vals[k]; ok && !copyKeyDropped(k) {
				fmt.Fprintf(&s, "%s = %s\n", k, v)
				delete(vals, k)
			}
		}
	} else {
		s.WriteString("description = copy fetched by pgCellar\nconninfo = dbname=postgres\nbackup_method = postgres\n")
	}
	conf := filepath.Join(dir, "barman.conf")
	if err := os.WriteFile(conf, []byte(g.String()), 0o640); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(confDir, name+".conf"), []byte(s.String()), 0o640); err != nil {
		return "", err
	}
	return conf, nil
}
