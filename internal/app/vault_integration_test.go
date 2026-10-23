package app_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/app"
	"github.com/dbfactory-io/pgcellar/internal/app/apptest"
	"github.com/dbfactory-io/pgcellar/internal/config"
	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// TestVaultIntegration runs the off-site copy against a real S3 service with
// the real restic: copy, copy again, restore by hand and compare, drill from
// the vault, forget and prune on a bucket with Object Lock. It is skipped
// unless PGCELLAR_TEST_S3_ENDPOINT is set; tools/vault-s3-test.sh starts a
// local MinIO under Podman (or Docker) and runs it:
//
//	PGCELLAR_TEST_S3_ENDPOINT    http://127.0.0.1:19000
//	PGCELLAR_TEST_S3_ACCESS_KEY  access key
//	PGCELLAR_TEST_S3_SECRET_KEY  secret key
//	PGCELLAR_TEST_S3_REGION      region (default us-east-1)
//	PGCELLAR_TEST_S3_BUCKET      an existing bucket; empty = the test creates
//	                             one with Object Lock (governance, 1 day)
//	PGCELLAR_TEST_RESTIC         the restic binary (default: restic on PATH)
func TestVaultIntegration(t *testing.T) {
	endpoint := os.Getenv("PGCELLAR_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("PGCELLAR_TEST_S3_ENDPOINT is not set (see tools/vault-s3-test.sh)")
	}
	restic := os.Getenv("PGCELLAR_TEST_RESTIC")
	if restic == "" {
		restic = "restic"
	}
	if _, err := exec.LookPath(restic); err != nil {
		t.Skipf("restic not found (%s)", restic)
	}
	access, secret := os.Getenv("PGCELLAR_TEST_S3_ACCESS_KEY"), os.Getenv("PGCELLAR_TEST_S3_SECRET_KEY")
	region := os.Getenv("PGCELLAR_TEST_S3_REGION")
	if region == "" {
		region = "us-east-1"
	}
	bucket := os.Getenv("PGCELLAR_TEST_S3_BUCKET")
	locked := false
	if bucket == "" {
		bucket = fmt.Sprintf("pgcellar-it-%d", time.Now().Unix())
		createLockedBucket(t, endpoint, region, bucket, access, secret)
		locked = true
	}

	e := apptest.NewWith(t, func(c *config.Config) {
		c.Vault = config.Vault{Enabled: true, Endpoint: endpoint, Region: region, Bucket: bucket, AccessKey: access}
	})
	a := e.App
	a.VaultTools = app.VaultTools{Restic: restic}
	cfg := a.Config()
	if err := os.MkdirAll(filepath.Join(cfg.DataDir, "vault"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Vault.EffectiveSecretKeyFile(cfg.DataDir), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	// Three base backups of 1 MiB each, next to the catalogue of the fixtures.
	srvDir := filepath.Join(e.Home, "pg-main")
	for _, id := range []string{"20261018T020001", "20261019T020001", "20261020T020001"} {
		dir := filepath.Join(srvDir, "base", id)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		data := make([]byte, 1<<20)
		_, _ = rand.Read(data)
		_ = os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o640)
		info, _ := os.ReadFile(filepath.Join(srvDir, "meta", id+"-backup.info"))
		_ = os.WriteFile(filepath.Join(dir, "backup.info"), info, 0o640)
	}
	ctx := context.Background()

	res, err := a.TestVault(ctx, "admin-user", nil)
	if err != nil || !res.OK {
		t.Fatalf("connection test: %+v %v", res, err)
	}
	if locked && (!res.Lock.Locked() || res.Lock.Days != 1) {
		t.Errorf("Object Lock not seen: %+v", res.Lock)
	}
	t.Logf("test: %s", res.Steps[0].Note)

	copyOnce := func() app.VaultCopyView {
		t.Helper()
		copyNow(t, a, "pg-main")
		a.VaultTick(ctx, a.Now())
		c := lastCopy(t, a, "pg-main")
		if !c.OK {
			t.Fatalf("copy failed: %s\n%s", c.Error, c.Journal)
		}
		return c
	}
	c1 := copyOnce()
	if c1.SnapshotID == "" || !c1.Initialised || c1.BytesTotal < 3<<20 {
		t.Fatalf("first copy: %+v", c1)
	}
	// Nothing new: no snapshot, or (when only the metadata of a parent
	// directory moved) one of a few KB.
	if c2 := copyOnce(); !c2.Unchanged && c2.BytesAdded > 64<<10 {
		t.Errorf("an unchanged directory sends almost nothing: %+v", c2)
	}
	wal := filepath.Join(srvDir, "wals", "0000000100000000", "000000010000000000000008")
	_ = os.MkdirAll(filepath.Dir(wal), 0o750)
	_ = os.WriteFile(wal, bytes.Repeat([]byte("w"), 16<<10), 0o640)
	if c3 := copyOnce(); c3.SnapshotID == "" || c3.Unchanged {
		t.Errorf("a new WAL file makes a snapshot: %+v", c3)
	}
	st, _ := a.State("pg-main")
	if st.Vault == nil || st.Vault.UsedBytes < 3<<20 {
		t.Errorf("usage measured on S3: %+v", st.Vault)
	}

	// Restore by hand, as the recovery key explains, and compare.
	target := t.TempDir()
	cmd := exec.Command(restic, "-r", vault.Repo(endpoint, bucket, "pgcellar", "pg-main"), "--password-file", cfg.Vault.EffectivePasswordFile(cfg.DataDir), "restore", "latest", "--target", target)
	cmd.Env = append(os.Environ(), "AWS_ACCESS_KEY_ID="+access, "AWS_SECRET_ACCESS_KEY="+secret, "AWS_DEFAULT_REGION="+region)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("restic restore by hand: %v\n%s", err, out)
	}
	compareTrees(t, srvDir, filepath.Join(target, srvDir))
	if conf, _ := os.ReadFile(filepath.Join(target, e.BarmanDir, "pg-main.conf")); len(conf) == 0 {
		t.Error("the barman.d file is not in the copy")
	}

	// Drill from the vault: the catalogue, then one backup and the WAL files.
	d := drillFromVault(t, e, app.DrillInput{})
	if !d.OK || d.BackupID != "20261020T020001" {
		t.Fatalf("drill from the vault: %+v\n%s", d.Details, d.Details.Error)
	}
	if f := d.Details.FetchedBytes; f < 1<<20 || f >= 2<<20 {
		t.Errorf("fetched %d bytes: one base backup of 1 MiB expected, the others excluded", f)
	}

	// Forget and prune once a day: force it now, on the locked bucket.
	if err := a.Store.SetSetting("vault.prune.pg-main", a.Now().Add(-48*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(wal+".2", []byte("x"), 0o640)
	if c := copyOnce(); !c.Pruned || c.PruneError != "" {
		t.Errorf("prune: %+v\n%s", c, c.Journal)
	}
	v := a.VaultStatus()
	t.Logf("bucket %s: %d bytes current, %d bytes of older versions, lock %q", bucket, v.UsedBytes-v.NoncurrentBytes, v.NoncurrentBytes, v.LockText)
}

func compareTrees(t *testing.T, want, got string) {
	t.Helper()
	n := 0
	err := filepath.Walk(want, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(want, p)
		a, _ := os.ReadFile(p)
		b, err := os.ReadFile(filepath.Join(got, rel))
		if err != nil || !bytes.Equal(a, b) {
			t.Errorf("%s differs after the round trip: %v", rel, err)
		}
		n++
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("compare: %d files, %v", n, err)
	}
}

// createLockedBucket creates a bucket with Object Lock and a default
// GOVERNANCE retention of one day.
func createLockedBucket(t *testing.T, endpoint, region, bucket, access, secret string) {
	t.Helper()
	send := func(method, query string, hdr map[string]string, body []byte) {
		req, _ := http.NewRequest(method, strings.TrimRight(endpoint, "/")+"/"+bucket+query, bytes.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		vault.Sign(req, body, access, secret, region, time.Now())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode/100 != 2 {
			t.Fatalf("%s %s: %d %s", method, query, resp.StatusCode, b)
		}
	}
	send(http.MethodPut, "", map[string]string{"X-Amz-Bucket-Object-Lock-Enabled": "true"}, nil)
	rule := []byte(`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>1</Days></DefaultRetention></Rule></ObjectLockConfiguration>`)
	sum := md5.Sum(rule)
	send(http.MethodPut, "?object-lock=", map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}, rule)
}
