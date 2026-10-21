// A stand-in for pgcellar used by the selfupdate_e2e test: `version` prints
// the version, anything else runs Startup, a /healthz, and the update loop
// against an archive served over HTTPS by the program itself.
//
// Environment: E2E_ARCHIVE (tar.gz to offer), E2E_TARGET (its version),
// E2E_CRASH_VERSION (this version exits 3 right after Startup), E2E_RUN_FOR
// (how long the loop runs, default 3s).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/selfupdate"
)

var Version = "dev"

type source struct{ rel selfupdate.Release }

func (s source) Latest() (selfupdate.Release, bool) { return s.rel, true }

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("pgcellar", Version)
		return
	}
	logger := log.New(os.Stdout, "["+Version+"] ", 0)
	sys := selfupdate.DefaultSystem()
	sys.InContainer = func() bool { return false }

	res := selfupdate.Startup(sys, Version, logger.Printf)
	logger.Printf("running %s", Version)
	if os.Getenv("E2E_CRASH_VERSION") == Version {
		logger.Printf("crashing on purpose")
		os.Exit(3)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		logger.Fatal(err)
	}
	go func() {
		_ = http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	}()

	archive, err := os.ReadFile(os.Getenv("E2E_ARCHIVE"))
	if err != nil {
		logger.Fatal(err)
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(archive) }))
	defer srv.Close()
	sys.HTTP = srv.Client()
	h := sha256.Sum256(archive)
	plat := sys.Platform

	u := &selfupdate.Updater{
		Current: Version,
		Enabled: true,
		Source: source{selfupdate.Release{Version: os.Getenv("E2E_TARGET"), Binaries: map[string]selfupdate.Binary{
			plat: {URL: srv.URL + "/pgcellar.tar.gz", SHA256: hex.EncodeToString(h[:])},
		}}},
		Logf:        logger.Printf,
		HealthURL:   "http://" + ln.Addr().String() + "/healthz",
		Confirm:     res.Confirm,
		Interval:    100 * time.Millisecond,
		SettleDelay: 300 * time.Millisecond,
		Sys:         sys,
	}
	runFor := 3 * time.Second
	if d, err := time.ParseDuration(os.Getenv("E2E_RUN_FOR")); err == nil {
		runFor = d
	}
	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()
	u.Run(ctx)
	b, _ := json.Marshal(u.Status())
	fmt.Printf("STATUS %s\n", b)
}
