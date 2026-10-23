package vault_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/vault"
	"github.com/dbfactory-io/pgcellar/internal/vault/s3test"
)

// The two examples of the AWS documentation "Signature calculations for the
// Authorization header: Transferring payload in a single chunk".
const (
	exampleAccess = "AKIAIOSFODNN7EXAMPLE"
	exampleSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

var exampleTime = time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)

func signature(h string) string {
	_, sig, _ := strings.Cut(h, "Signature=")
	return sig
}

func TestSignGetObjectExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	vault.Sign(req, nil, exampleAccess, exampleSecret, "us-east-1", exampleTime)
	auth := req.Header.Get("Authorization")
	if got := signature(auth); got != "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41" {
		t.Errorf("signature = %s\n%s", got, auth)
	}
	if !strings.Contains(auth, "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date") {
		t.Errorf("signed headers: %s", auth)
	}
}

func TestSignPutObjectExample(t *testing.T) {
	body := []byte("Welcome to Amazon S3.")
	req, _ := http.NewRequest(http.MethodPut, "https://examplebucket.s3.amazonaws.com/test%24file.text", nil)
	req.Header.Set("Date", "Fri, 24 May 2013 00:00:00 GMT")
	req.Header.Set("X-Amz-Storage-Class", "REDUCED_REDUNDANCY")
	vault.Sign(req, body, exampleAccess, exampleSecret, "us-east-1", exampleTime)
	if got := signature(req.Header.Get("Authorization")); got != "98ad721746da40c64f1a55b78f14c238d841ea1380cd77a1b5971af0ece108bd" {
		t.Errorf("signature = %s", got)
	}
	if req.Header.Get("X-Amz-Content-Sha256") != "44ce7dd67c959e0d3524ffac1771dfbba87d2b6b4b4e99e42034a8b803f8b072" {
		t.Errorf("payload hash = %s", req.Header.Get("X-Amz-Content-Sha256"))
	}
}

func TestClientAgainstFake(t *testing.T) {
	srv := s3test.New()
	defer srv.Close()
	c := srv.Client()
	ctx := context.Background()

	if err := c.Put(ctx, "pgcellar/.test/a b+c", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(ctx, "pgcellar/.test/a b+c")
	if err != nil || string(got) != "hello" {
		t.Fatalf("get = %q, %v", got, err)
	}
	lock, err := c.ObjectLock(ctx)
	if err != nil || lock.Enabled || lock.Locked() {
		t.Fatalf("no lock expected: %+v %v", lock, err)
	}
	srv.SetLock(30)
	lock, err = c.ObjectLock(ctx)
	if err != nil || !lock.Locked() || lock.Days != 30 || lock.Mode != "GOVERNANCE" {
		t.Fatalf("lock = %+v %v", lock, err)
	}
	// With a lock, a write needs Content-MD5: the client always sends it.
	if err := c.Put(ctx, "pgcellar/srv/data/1", make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	if err := c.Put(ctx, "pgcellar/srv/data/1", make([]byte, 400)); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, "pgcellar/.test/a b+c"); err != nil {
		t.Fatal(err)
	}
	u, err := c.Usage(ctx, "pgcellar/srv/")
	if err != nil {
		t.Fatal(err)
	}
	if !u.Versions || u.CurrentBytes != 400 || u.NoncurrentBytes != 1000 || u.Objects != 1 || u.Total() != 1400 {
		t.Errorf("usage = %+v", u)
	}
	srv.NoVersions = true
	u, err = c.Usage(ctx, "pgcellar/srv/")
	if err != nil || u.Versions || u.CurrentBytes != 400 {
		t.Errorf("usage without versions = %+v %v", u, err)
	}

	bad := *c
	bad.SecretKey = "wrong"
	err = bad.Put(ctx, "x", []byte("x"))
	if !vault.IsCode(err, "SignatureDoesNotMatch") || !strings.Contains(err.Error(), "secret key does not match") {
		t.Errorf("wrong secret: %v", err)
	}
	other := *c
	other.Bucket = "nope"
	if _, err := other.Get(ctx, "x"); !vault.IsCode(err, "NoSuchBucket") {
		t.Errorf("missing bucket: %v", err)
	}
}
