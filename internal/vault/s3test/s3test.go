// Package s3test is a small in-memory S3 service for tests: one bucket,
// objects with versions, an optional Object Lock rule, and a check of the
// signature of every request.
package s3test

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dbfactory-io/pgcellar/internal/vault"
)

// Server is the fake service. Its fields can be changed between requests.
type Server struct {
	*httptest.Server
	Bucket, Region, AccessKey, SecretKey string

	mu sync.Mutex
	// LockDays > 0 gives the bucket a default GOVERNANCE retention.
	LockDays int
	// NoVersions makes the version listing answer 501, as some services do.
	NoVersions bool
	// FailAll answers every request with this S3 error code (403).
	FailAll  string
	objects  map[string][]version // key -> versions, oldest first
	Requests []string             // "METHOD path?query" of every request
}

type version struct {
	data    []byte
	deleted bool // a delete marker
}

// New starts the fake service.
func New() *Server {
	s := &Server{Bucket: "test-bucket", Region: "test-1", AccessKey: "AKTEST", SecretKey: "secret-test-key", objects: map[string][]version{}}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Client returns a vault.S3 client aimed at the fake service.
func (s *Server) Client() *vault.S3 {
	return &vault.S3{Endpoint: s.URL, Region: s.Region, Bucket: s.Bucket, AccessKey: s.AccessKey, SecretKey: s.SecretKey}
}

// SetLock sets the default retention (0 = no Object Lock rule).
func (s *Server) SetLock(days int) { s.mu.Lock(); s.LockDays = days; s.mu.Unlock() }

// PutObject stores an object directly (test setup).
func (s *Server) PutObject(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects[key] = append(s.objects[key], version{data: data})
}

// Keys lists the current keys under a prefix.
func (s *Server) Keys(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k, vs := range s.objects {
		if strings.HasPrefix(k, prefix) && !vs[len(vs)-1].deleted {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func fail(w http.ResponseWriter, status int, code string) {
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, "<Error><Code>%s</Code><Message>%s</Message></Error>", code, code)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = append(s.Requests, r.Method+" "+r.URL.RequestURI())
	if s.FailAll != "" {
		fail(w, http.StatusForbidden, s.FailAll)
		return
	}
	// Signature: sign a copy the same way and compare.
	auth := r.Header.Get("Authorization")
	if !strings.Contains(auth, "Credential="+s.AccessKey+"/") {
		fail(w, http.StatusForbidden, "InvalidAccessKeyId")
		return
	}
	when, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		fail(w, http.StatusForbidden, "AccessDenied")
		return
	}
	cp := r.Clone(r.Context())
	cp.Header = http.Header{}
	for k, v := range r.Header {
		switch strings.ToLower(k) {
		case "authorization", "x-amz-date", "x-amz-content-sha256", "user-agent", "accept-encoding", "content-length":
		default:
			cp.Header[k] = v
		}
	}
	vault.Sign(cp, body, s.AccessKey, s.SecretKey, s.Region, when)
	if cp.Header.Get("Authorization") != auth {
		fail(w, http.StatusForbidden, "SignatureDoesNotMatch")
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != s.Bucket {
		fail(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	q := r.URL.Query()
	switch {
	case key == "" && q.Has("object-lock"):
		if s.LockDays <= 0 {
			fail(w, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
			return
		}
		_, _ = fmt.Fprintf(w, "<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled><Rule><DefaultRetention><Mode>GOVERNANCE</Mode><Days>%d</Days></DefaultRetention></Rule></ObjectLockConfiguration>", s.LockDays)
	case key == "" && q.Has("versions"):
		if s.NoVersions {
			fail(w, http.StatusNotImplemented, "NotImplemented")
			return
		}
		s.listVersions(w, q.Get("prefix"))
	case key == "" && q.Get("list-type") == "2":
		s.listObjects(w, q.Get("prefix"))
	case key == "":
		fail(w, http.StatusBadRequest, "InvalidRequest")
	case r.Method == http.MethodPut:
		if s.LockDays > 0 {
			sum := md5.Sum(body)
			if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) {
				fail(w, http.StatusBadRequest, "InvalidRequest")
				return
			}
		}
		s.objects[key] = append(s.objects[key], version{data: body})
	case r.Method == http.MethodGet:
		vs := s.objects[key]
		if len(vs) == 0 || vs[len(vs)-1].deleted {
			fail(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = io.Copy(w, bytes.NewReader(vs[len(vs)-1].data))
	case r.Method == http.MethodDelete:
		if _, ok := s.objects[key]; ok {
			s.objects[key] = append(s.objects[key], version{deleted: true})
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		fail(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

type xmlVersion struct {
	Key    string `xml:"Key"`
	Latest bool   `xml:"IsLatest"`
	Size   int64  `xml:"Size"`
}

func (s *Server) listVersions(w http.ResponseWriter, prefix string) {
	type result struct {
		XMLName   xml.Name     `xml:"ListVersionsResult"`
		Truncated bool         `xml:"IsTruncated"`
		Versions  []xmlVersion `xml:"Version"`
	}
	var res result
	for k, vs := range s.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		for i, v := range vs {
			if v.deleted {
				continue
			}
			res.Versions = append(res.Versions, xmlVersion{Key: k, Latest: i == len(vs)-1, Size: int64(len(v.data))})
		}
	}
	_ = xml.NewEncoder(w).Encode(res)
}

func (s *Server) listObjects(w http.ResponseWriter, prefix string) {
	type obj struct {
		Key  string `xml:"Key"`
		Size int64  `xml:"Size"`
	}
	type result struct {
		XMLName   xml.Name `xml:"ListBucketResult"`
		Truncated bool     `xml:"IsTruncated"`
		Contents  []obj    `xml:"Contents"`
	}
	var res result
	for k, vs := range s.objects {
		if strings.HasPrefix(k, prefix) && !vs[len(vs)-1].deleted {
			res.Contents = append(res.Contents, obj{Key: k, Size: int64(len(vs[len(vs)-1].data))})
		}
	}
	_ = xml.NewEncoder(w).Encode(res)
}
