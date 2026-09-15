// Package s3p deploys Maven artifacts to Exoscale Object Storage through
// the s3p:// scheme tools.project's deps-deploy used
// (s3p://<bucket>/<prefix>). It mirrors the sos-wagon-private wagon that
// tools.project deployed through:
//   - endpoint https://sos-ch-dk-2.exo.io, region ch-dk-2 (hardcoded in
//     the wagon),
//   - object keys <prefix>/<maven layout path> (e.g.
//     releases/org/exoscale/app/1.0.0/app-1.0.0.jar),
//   - Content-Type application/octet-stream on every object,
//   - .sha1/.md5 checksums next to the jar and pom (Maven layout),
//   - credentials from the settings.xml server entry of the repository id,
//     else the standard AWS chain (AWS_ACCESS_KEY_ID /
//     AWS_SECRET_ACCESS_KEY env, ~/.aws/credentials, AWS_PROFILE).
package s3p

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// The endpoint and signing region the legacy Exoscale wagon
	// (sos-wagon-private) used unconditionally for s3p:// URLs.
	endpoint = "sos-ch-dk-2.exo.io"
	region   = "ch-dk-2"
	service  = "s3"
)

// Target is a parsed s3p:// URL.
type Target struct {
	Bucket string
	Prefix string // no leading or trailing slash, "" when absent
}

// Object is one object to upload: its key in the bucket and either its local
// path or its content in memory.
type Object struct {
	Key   string
	Local string
	Data  []byte
}

// Parse parses "s3p://<bucket>/<prefix>".
func Parse(s string) (*Target, error) {
	rest, ok := strings.CutPrefix(s, "s3p://")
	if !ok {
		return nil, fmt.Errorf("not an s3p url: %s", s)
	}
	bucket, prefix, _ := strings.Cut(rest, "/")
	if bucket == "" {
		return nil, fmt.Errorf("s3p url has no bucket: %s", s)
	}
	return &Target{Bucket: bucket, Prefix: strings.Trim(prefix, "/")}, nil
}

// Creds are the AWS credentials to sign requests with.
type Creds struct {
	AccessKey string
	SecretKey string
	Token     string
}

// Credentials resolves the deploy credentials: the settings.xml server
// entry for the repo id first (username -> access key, password -> secret,
// how deps-deploy wired it), then the standard AWS chain.
func Credentials(settingsUser, settingsPass string) (Creds, error) {
	if settingsUser != "" && settingsPass != "" {
		return Creds{AccessKey: settingsUser, SecretKey: settingsPass}, nil
	}
	ak, sk, ok := envCreds()
	if ok {
		return Creds{AccessKey: ak, SecretKey: sk, Token: os.Getenv("AWS_SESSION_TOKEN")}, nil
	}
	profile := os.Getenv("AWS_PROFILE")
	if profile == "" {
		profile = "default"
	}
	home, _ := os.UserHomeDir()
	credFile := envOr("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(home, ".aws", "credentials"))
	cfgFile := envOr("AWS_CONFIG_FILE", filepath.Join(home, ".aws", "config"))
	for _, p := range []struct {
		path, section, ak, sk, tok string
	}{
		{credFile, profile, "aws_access_key_id", "aws_secret_access_key", "aws_session_token"},
		{credFile, profile, "access_key", "secret_key", ""},
		{cfgFile, "profile " + profile, "aws_access_key_id", "aws_secret_access_key", "aws_session_token"},
		{cfgFile, "default", "aws_access_key_id", "aws_secret_access_key", "aws_session_token"},
	} {
		if v, ok := iniGet(p.path, p.section, p.ak); ok && v != "" {
			if s, ok := iniGet(p.path, p.section, p.sk); ok && s != "" {
				tok := ""
				if p.tok != "" {
					tok, _ = iniGet(p.path, p.section, p.tok)
				}
				return Creds{AccessKey: v, SecretKey: s, Token: tok}, nil
			}
		}
	}
	return Creds{}, fmt.Errorf("no AWS credentials for the s3p deploy (settings.xml server, AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or ~/.aws/credentials)")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envCreds() (string, string, bool) {
	ak, sk := os.Getenv("AWS_ACCESS_KEY_ID"), os.Getenv("AWS_SECRET_ACCESS_KEY")
	return ak, sk, ak != "" && sk != ""
}

// iniGet returns the value of key in the section of an INI file,
// "" and false when absent.
func iniGet(path, section, key string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sec := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sec = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(k) == key && strings.TrimSpace(sec) == section {
			return strings.TrimSpace(v), true
		}
	}
	return "", false
}

// Upload puts every object to the target, signing each request with
// AWS Signature Version 4.
func Upload(ctx context.Context, t *Target, c Creds, objs []Object) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, o := range objs {
		if err := put(ctx, client, t, c, o); err != nil {
			return fmt.Errorf("put %s: %w", o.Key, err)
		}
	}
	return nil
}

func put(ctx context.Context, client *http.Client, t *Target, c Creds, o Object) error {
	var (
		src  io.ReadSeeker
		size int64
	)
	if len(o.Data) > 0 {
		src, size = bytes.NewReader(o.Data), int64(len(o.Data))
	} else {
		f, err := os.Open(o.Local)
		if err != nil {
			return err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return fmt.Errorf("not a regular file: %s", o.Local)
		}
		src, size = f, st.Size()
	}
	host := t.Bucket + "." + endpoint
	path := escapePath(o.Key)
	for attempt := 0; ; attempt++ {
		err := func() error {
			amzDate := time.Now().UTC().Format("20060102T150405Z")
			auth, err := signRequest(c, amzDate, host, path)
			if err != nil {
				return err
			}
			if _, err := src.Seek(0, io.SeekStart); err != nil {
				return err
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPut, "https://"+host+path, src)
			if err != nil {
				return err
			}
			req.ContentLength = size
			req.Header.Set("Content-Type", "application/octet-stream")
			req.Header.Set("X-Amz-Date", amzDate)
			req.Header.Set("X-Amz-Content-Sha256", "UNSIGNED-PAYLOAD")
			req.Header.Set("Authorization", auth)
			if c.Token != "" {
				req.Header.Set("X-Amz-Security-Token", c.Token)
			}
			resp, err := client.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				return s3Error(resp)
			}
			return nil
		}()
		if err == nil {
			return nil
		}
		if !retryable(ctx, err, attempt) {
			return err
		}
	}
}

// signRequest builds the Authorization header for a PUT of payload-hash
// UNSIGNED-PAYLOAD to https://host+path.
func signRequest(c Creds, amzDate, host, path string) (string, error) {
	payloadHash := "UNSIGNED-PAYLOAD"
	var headers, signed []string
	add := func(k, v string) {
		headers = append(headers, k+":"+v)
		signed = append(signed, k)
	}
	add("content-type", "application/octet-stream")
	add("host", host)
	add("x-amz-content-sha256", payloadHash)
	add("x-amz-date", amzDate)
	if c.Token != "" {
		add("x-amz-security-token", c.Token)
	}
	sort.Strings(signed)
	canonical := strings.Join([]string{
		http.MethodPut,
		path,
		"", // no query
		strings.Join(headers, "\n") + "\n",
		strings.Join(signed, ";"),
		payloadHash,
	}, "\n")
	date := amzDate[:8]
	scope := date + "/" + region + "/" + service + "/request"
	sum := sha256.Sum256([]byte(canonical))
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		hex.EncodeToString(sum[:]),
	}, "\n")
	kDate := hmacSHA256([]byte("AWS4"+c.SecretKey), []byte(date))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("request"))
	sig := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))
	return fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, strings.Join(signed, ";"), sig), nil
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}

// escapePath URL-escapes every segment of a bucket key, keeping the
// separators (a key is always absolute: it starts with no slash here,
// callers prepend none; put() adds the leading slash via path).
func escapePath(key string) string {
	segs := strings.Split(key, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

func retryable(ctx context.Context, err error, attempt int) bool {
	if ctx.Err() != nil || attempt >= 3 {
		return false
	}
	var se *s3Err
	if errors.As(err, &se) && se.status >= 400 && se.status < 500 && se.status != http.StatusTooManyRequests {
		return false
	}
	time.Sleep(time.Duration(250*(1<<attempt)) * time.Millisecond)
	return true
}

type s3Err struct {
	status  int
	code    string
	message string
}

func (e *s3Err) Error() string {
	if e.code != "" {
		return fmt.Sprintf("s3: %s (status %d): %s", e.code, e.status, e.message)
	}
	return fmt.Sprintf("s3: status %d: %s", e.status, e.message)
}

// s3Error parses an S3 error response body into an *s3Err.
func s3Error(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(body, &e)
	if e.Code == "" {
		e.Code = strings.TrimSpace(string(body))
		if len(e.Code) > 200 {
			e.Code = e.Code[:200]
		}
	}
	return &s3Err{status: resp.StatusCode, code: e.Code, message: e.Message}
}

// ChecksumsOf returns the hex sha1 and md5 digests of the file at path.
func ChecksumsOf(path string) (sha1hex, md5hex string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	s1, s5 := sha1.New(), md5.New()
	if _, err := io.Copy(io.MultiWriter(s1, s5), f); err != nil {
		return "", "", err
	}
	return hex.EncodeToString(s1.Sum(nil)), hex.EncodeToString(s5.Sum(nil)), nil
}
