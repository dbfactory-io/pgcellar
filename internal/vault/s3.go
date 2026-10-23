// Package vault holds what the off-site copy needs besides restic itself: a
// small S3 client (signature version 4, path-style requests, standard library
// only) to test a bucket, read its Object Lock rule and measure what a prefix
// takes, plus the restic command lines and the parsing of their JSON output.
//
// restic does the copy, the encryption and the restore; this package never
// moves backup data.
package vault

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// S3 is a client for one bucket of an S3-compatible service.
type S3 struct {
	Endpoint  string // https://s3.gra.io.cloud.ovh.net
	Region    string // gra
	Bucket    string
	AccessKey string
	SecretKey string
	// HTTP is the client to use; nil = a client with a 60 s timeout.
	HTTP *http.Client
	// Now replaces the clock of the signature (tests).
	Now func() time.Time
}

// Error is an answer of the S3 service that is not a success.
type Error struct {
	Status  int
	Code    string // AccessDenied, NoSuchBucket, SignatureDoesNotMatch...
	Message string
}

func (e *Error) Error() string {
	if hint := hints[e.Code]; hint != "" {
		return fmt.Sprintf("%s (%s, HTTP %d)", hint, e.Code, e.Status)
	}
	msg := strings.TrimSpace(e.Message)
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (%s, HTTP %d)", msg, e.Code, e.Status)
	}
	return fmt.Sprintf("%s (HTTP %d)", msg, e.Status)
}

// hints say in plain words what the usual error codes mean.
var hints = map[string]string{
	"AccessDenied":                 "the S3 user is not allowed to do this on the bucket: give it read and write rights on the bucket",
	"InvalidAccessKeyId":           "the access key is unknown to the S3 service: check vault.access_key and the region",
	"SignatureDoesNotMatch":        "the secret key does not match the access key: check the secret key and the region",
	"NoSuchBucket":                 "the bucket does not exist in this region: create it first, or check vault.bucket and the endpoint",
	"AuthorizationHeaderMalformed": "the region does not match the endpoint: check vault.region",
	"RequestTimeTooSkewed":         "the clock of this machine is too far from the time of the S3 service: fix the time (NTP)",
}

// IsCode reports whether err is an S3 answer with that code.
func IsCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

func (c *S3) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *S3) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func (c *S3) region() string {
	if c.Region == "" {
		return "us-east-1"
	}
	return c.Region
}

// objectURL is the path-style address of a key ("" = the bucket itself).
func (c *S3) objectURL(key string, query url.Values) (*url.URL, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("endpoint %q must start with https://", c.Endpoint)
	}
	p := "/" + c.Bucket
	if key != "" {
		p += "/" + key
	}
	u.Path = p
	u.RawPath = uriEncode(p, false)
	u.RawQuery = canonicalQuery(query)
	return u, nil
}

// do sends one signed request and returns the body of a 2xx answer.
func (c *S3) do(ctx context.Context, method, key string, query url.Values, body []byte, md5sum bool) ([]byte, http.Header, error) {
	u, err := c.objectURL(key, query)
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.URL = u // keep the exact encoding that was signed
	req.ContentLength = int64(len(body))
	if md5sum {
		sum := md5.Sum(body)
		req.Header.Set("Content-MD5", base64.StdEncoding.EncodeToString(sum[:]))
	}
	Sign(req, body, c.AccessKey, c.SecretKey, c.region(), c.now())
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, resp.Header, parseError(resp.StatusCode, data)
	}
	return data, resp.Header, nil
}

func parseError(status int, body []byte) error {
	var x struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &x)
	return &Error{Status: status, Code: x.Code, Message: x.Message}
}

// Put writes an object. Content-MD5 is always sent: a bucket with Object Lock
// refuses a write without it.
func (c *S3) Put(ctx context.Context, key string, data []byte) error {
	_, _, err := c.do(ctx, http.MethodPut, key, nil, data, true)
	return err
}

// Get reads an object.
func (c *S3) Get(ctx context.Context, key string) ([]byte, error) {
	data, _, err := c.do(ctx, http.MethodGet, key, nil, nil, false)
	return data, err
}

// Delete removes an object. On a versioned bucket it only adds a delete
// marker; the version stays (and is billed) until its retention ends.
func (c *S3) Delete(ctx context.Context, key string) error {
	_, _, err := c.do(ctx, http.MethodDelete, key, nil, nil, false)
	return err
}

// Lock is the Object Lock rule of a bucket.
type Lock struct {
	Enabled bool   `json:"enabled"`        // the bucket was created with Object Lock
	Mode    string `json:"mode,omitempty"` // GOVERNANCE or COMPLIANCE, empty without a default retention
	Days    int    `json:"days,omitempty"` // default retention, in days (years are converted)
}

// Locked reports whether new objects get a retention by default.
func (l Lock) Locked() bool { return l.Enabled && l.Mode != "" && l.Days > 0 }

// ObjectLock reads the Object Lock rule of the bucket. A bucket without one
// answers ObjectLockConfigurationNotFoundError, which is not an error here.
func (c *S3) ObjectLock(ctx context.Context) (Lock, error) {
	data, _, err := c.do(ctx, http.MethodGet, "", url.Values{"object-lock": {""}}, nil, false)
	if err != nil {
		if IsCode(err, "ObjectLockConfigurationNotFoundError") || IsCode(err, "NoSuchObjectLockConfiguration") {
			return Lock{}, nil
		}
		return Lock{}, err
	}
	var x struct {
		Enabled string `xml:"ObjectLockEnabled"`
		Rule    struct {
			Default struct {
				Mode  string `xml:"Mode"`
				Days  int    `xml:"Days"`
				Years int    `xml:"Years"`
			} `xml:"DefaultRetention"`
		} `xml:"Rule"`
	}
	if err := xml.Unmarshal(data, &x); err != nil {
		return Lock{}, fmt.Errorf("the Object Lock answer is not readable: %w", err)
	}
	l := Lock{Enabled: strings.EqualFold(x.Enabled, "Enabled"), Mode: strings.ToUpper(x.Rule.Default.Mode), Days: x.Rule.Default.Days}
	if x.Rule.Default.Years > 0 {
		l.Days = x.Rule.Default.Years * 365
	}
	return l, nil
}

// Usage is what the objects under a prefix take.
type Usage struct {
	// CurrentBytes is the size of the objects that exist now.
	CurrentBytes int64 `json:"current_bytes"`
	// NoncurrentBytes is the size of older versions: objects restic deleted
	// that the bucket keeps (versioning, Object Lock). They are billed too.
	NoncurrentBytes int64 `json:"noncurrent_bytes"`
	Objects         int   `json:"objects"`
	// Versions is false when the service would not list versions: only the
	// current objects were counted.
	Versions bool `json:"versions"`
}

// Total is everything the prefix is billed for.
func (u Usage) Total() int64 { return u.CurrentBytes + u.NoncurrentBytes }

// Usage measures a prefix by listing its object versions, or its objects
// when the service does not list versions.
func (c *S3) Usage(ctx context.Context, prefix string) (Usage, error) {
	u, err := c.usageVersions(ctx, prefix)
	if err == nil {
		return u, nil
	}
	var se *Error
	if !errors.As(err, &se) || (se.Status != http.StatusForbidden && se.Status != http.StatusNotImplemented && se.Status != http.StatusBadRequest) {
		return Usage{}, err
	}
	return c.usageObjects(ctx, prefix)
}

func (c *S3) usageVersions(ctx context.Context, prefix string) (Usage, error) {
	u := Usage{Versions: true}
	q := url.Values{"versions": {""}, "prefix": {prefix}, "max-keys": {"1000"}}
	for page := 0; page < 100000; page++ {
		data, _, err := c.do(ctx, http.MethodGet, "", q, nil, false)
		if err != nil {
			return Usage{}, err
		}
		var x struct {
			Truncated bool   `xml:"IsTruncated"`
			NextKey   string `xml:"NextKeyMarker"`
			NextVerID string `xml:"NextVersionIdMarker"`
			Versions  []struct {
				Latest bool  `xml:"IsLatest"`
				Size   int64 `xml:"Size"`
			} `xml:"Version"`
		}
		if err := xml.Unmarshal(data, &x); err != nil {
			return Usage{}, fmt.Errorf("the version list is not readable: %w", err)
		}
		for _, v := range x.Versions {
			if v.Latest {
				u.CurrentBytes += v.Size
				u.Objects++
			} else {
				u.NoncurrentBytes += v.Size
			}
		}
		if !x.Truncated || x.NextKey == "" {
			return u, nil
		}
		q.Set("key-marker", x.NextKey)
		if x.NextVerID != "" {
			q.Set("version-id-marker", x.NextVerID)
		} else {
			q.Del("version-id-marker")
		}
	}
	return u, nil
}

func (c *S3) usageObjects(ctx context.Context, prefix string) (Usage, error) {
	var u Usage
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
	for page := 0; page < 100000; page++ {
		data, _, err := c.do(ctx, http.MethodGet, "", q, nil, false)
		if err != nil {
			return Usage{}, err
		}
		var x struct {
			Truncated bool   `xml:"IsTruncated"`
			Next      string `xml:"NextContinuationToken"`
			Contents  []struct {
				Size int64 `xml:"Size"`
			} `xml:"Contents"`
		}
		if err := xml.Unmarshal(data, &x); err != nil {
			return Usage{}, fmt.Errorf("the object list is not readable: %w", err)
		}
		for _, o := range x.Contents {
			u.CurrentBytes += o.Size
			u.Objects++
		}
		if !x.Truncated || x.Next == "" {
			return u, nil
		}
		q.Set("continuation-token", x.Next)
	}
	return u, nil
}

// ---- signature version 4 ----

const unsignedTime = "20060102T150405Z"

// Sign adds the AWS signature version 4 headers to req: Host, X-Amz-Date,
// X-Amz-Content-Sha256 and Authorization. Every header already set on the
// request (Content-MD5, Range...) is signed too.
func Sign(req *http.Request, body []byte, accessKey, secretKey, region string, now time.Time) {
	now = now.UTC()
	amzDate := now.Format(unsignedTime)
	day := now.Format("20060102")
	sum := sha256.Sum256(body)
	payload := hex.EncodeToString(sum[:])
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payload)
	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}

	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "user-agent" || lk == "content-length" {
			continue
		}
		headers[lk] = strings.Join(strings.Fields(strings.Join(v, ",")), " ")
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{
		req.Method,
		path,
		canonicalQuery(req.URL.Query()),
		canonHeaders.String(),
		signed,
		payload,
	}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	crSum := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(crSum[:])

	key := hmacSHA256([]byte("AWS4"+secretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", accessKey, scope, signed, sig))
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

// canonicalQuery sorts and encodes the query the way the signature wants it
// ("object-lock=" for a key without value).
func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for _, v := range vals {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

// uriEncode is the encoding of the signature: every byte but the unreserved
// characters is %XX, and "/" too unless it separates path segments.
func uriEncode(s string, slash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9', ch == '-', ch == '_', ch == '.', ch == '~':
			b.WriteByte(ch)
		case ch == '/' && !slash:
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}
