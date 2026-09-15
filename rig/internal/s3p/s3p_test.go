package s3p

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in      string
		bucket  string
		prefix  string
		wantErr bool
	}{
		{"s3p://exo-artifacts", "exo-artifacts", "", false},
		{"s3p://exo-artifacts/releases", "exo-artifacts", "releases", false},
		{"s3p://exo-artifacts/releases/", "exo-artifacts", "releases", false},
		{"s3p://exo-artifacts/a/b", "exo-artifacts", "a/b", false},
		{"s3p://", "", "", true},
		{"https://exo-artifacts", "", "", true},
	}
	for _, c := range cases {
		got, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) error: %v", c.in, err)
			continue
		}
		if got.Bucket != c.bucket || got.Prefix != c.prefix {
			t.Errorf("Parse(%q) = %q %q, want %q %q", c.in, got.Bucket, got.Prefix, c.bucket, c.prefix)
		}
	}
}

func TestEscapePath(t *testing.T) {
	if got := escapePath("releases/org/exoscale/app-1.0.0.jar"); got != "/releases/org/exoscale/app-1.0.0.jar" {
		t.Errorf("escapePath plain = %q", got)
	}
	if got := escapePath("releases/my app (x)/a b.txt"); got != "/releases/my%20app%20%28x%29/a%20b.txt" {
		t.Errorf("escapePath special = %q", got)
	}
}

// setCredsEnv blanks the credential environment and points the AWS files at
// empty temp paths, so each test controls the whole chain.
func setCredsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
}

func writeINI(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCredentials(t *testing.T) {
	setCredsEnv(t)
	credFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	cfgFile := os.Getenv("AWS_CONFIG_FILE")

	// The settings.xml server entry wins over everything else.
	c, err := Credentials("AK-SET", "SK-SET")
	if err != nil || c.AccessKey != "AK-SET" || c.SecretKey != "SK-SET" || c.Token != "" {
		t.Fatalf("settings: %+v %v", c, err)
	}

	// A server entry without a password falls through to the environment.
	t.Setenv("AWS_ACCESS_KEY_ID", "AK-ENV")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SK-ENV")
	t.Setenv("AWS_SESSION_TOKEN", "TOK-ENV")
	c, err = Credentials("AK-SET", "")
	if err != nil || c.AccessKey != "AK-ENV" || c.SecretKey != "SK-ENV" || c.Token != "TOK-ENV" {
		t.Fatalf("env: %+v %v", c, err)
	}

	// Standard keys in ~/.aws/credentials (default profile).
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("AWS_SESSION_TOKEN", "")
	writeINI(t, credFile, "[default]\naws_access_key_id = AK-CRED\naws_secret_access_key = SK-CRED\n")
	c, err = Credentials("", "")
	if err != nil || c.AccessKey != "AK-CRED" || c.SecretKey != "SK-CRED" {
		t.Fatalf("credentials file: %+v %v", c, err)
	}

	// The session token is picked up from the same section.
	writeINI(t, credFile, "[default]\naws_access_key_id = AK-CRED\naws_secret_access_key = SK-CRED\naws_session_token = TOK-CRED\n")
	c, err = Credentials("", "")
	if err != nil || c.Token != "TOK-CRED" {
		t.Fatalf("credentials token: %+v %v", c, err)
	}

	// Legacy key names.
	writeINI(t, credFile, "[default]\naccess_key = AK-LEG\nsecret_key = SK-LEG\n")
	c, err = Credentials("", "")
	if err != nil || c.AccessKey != "AK-LEG" || c.SecretKey != "SK-LEG" {
		t.Fatalf("legacy keys: %+v %v", c, err)
	}

	// A named profile in ~/.aws/config.
	t.Setenv("AWS_PROFILE", "myprof")
	writeINI(t, cfgFile, "[profile myprof]\naws_access_key_id = AK-MP\naws_secret_access_key = SK-MP\n")
	c, err = Credentials("", "")
	if err != nil || c.AccessKey != "AK-MP" || c.SecretKey != "SK-MP" {
		t.Fatalf("config profile: %+v %v", c, err)
	}

	// The [default] config section is the last resort for a missing profile.
	t.Setenv("AWS_PROFILE", "other")
	writeINI(t, cfgFile, "[default]\naws_access_key_id = AK-DEF\naws_secret_access_key = SK-DEF\n")
	c, err = Credentials("", "")
	if err != nil || c.AccessKey != "AK-DEF" || c.SecretKey != "SK-DEF" {
		t.Fatalf("config default: %+v %v", c, err)
	}

	// No source at all.
	writeINI(t, credFile, "[wrong]\naws_access_key_id = AK-NIL\naws_secret_access_key = SK-NIL\n")
	writeINI(t, cfgFile, "[profile wrong]\naws_access_key_id = AK-NIL\naws_secret_access_key = SK-NIL\n")
	if _, err = Credentials("", ""); err == nil || !strings.Contains(err.Error(), "no AWS credentials") {
		t.Fatalf("none: %v", err)
	}
}

func TestChecksumsOf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abc")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	s1, s5, err := ChecksumsOf(path)
	if err != nil {
		t.Fatal(err)
	}
	if s1 != "a9993e364706816aba3e25717850c26c9cd0d89d" || s5 != "900150983cd24fb0d6963f7d28e17f72" {
		t.Fatalf("digests: %s %s", s1, s5)
	}
	if _, _, err = ChecksumsOf(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing file: want error")
	}
}

func TestSignRequest(t *testing.T) {
	amzDate := "20260919T120000Z"
	host := "exo-artifacts.sos-ch-dk-2.exo.io"
	path := "/releases/org/exoscale/app/1.0.0/app-1.0.0.jar"
	creds := Creds{AccessKey: "AKIATEST", SecretKey: "secretkey"}

	got, err := signRequest(creds, amzDate, host, path)
	if err != nil {
		t.Fatal(err)
	}

	// Recomputed independently from the AWS SigV4 spec.
	canonical := "PUT\n" +
		path + "\n" +
		"\n" +
		"content-type:application/octet-stream\n" +
		"host:" + host + "\n" +
		"x-amz-content-sha256:UNSIGNED-PAYLOAD\n" +
		"x-amz-date:" + amzDate + "\n" +
		"\n" +
		"content-type;host;x-amz-content-sha256;x-amz-date\n" +
		"UNSIGNED-PAYLOAD"
	sum := sha256.Sum256([]byte(canonical))
	scope := "20260919/ch-dk-2/s3/request"
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(sum[:])
	k := hmac.New(sha256.New, []byte("AWS4"+creds.SecretKey))
	k.Write([]byte("20260919"))
	kRegion := hmac.New(sha256.New, k.Sum(nil))
	kRegion.Write([]byte("ch-dk-2"))
	kService := hmac.New(sha256.New, kRegion.Sum(nil))
	kService.Write([]byte("s3"))
	kSigning := hmac.New(sha256.New, kService.Sum(nil))
	kSigning.Write([]byte("request"))
	sig := hmac.New(sha256.New, kSigning.Sum(nil))
	sig.Write([]byte(stringToSign))
	want := "AWS4-HMAC-SHA256 Credential=AKIATEST/" + scope +
		", SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date, Signature=" +
		hex.EncodeToString(sig.Sum(nil))
	if got != want {
		t.Fatalf("authorization header:\n got %s\nwant %s", got, want)
	}

	again, _ := signRequest(creds, amzDate, host, path)
	if again != got {
		t.Fatal("not deterministic")
	}
	other, _ := signRequest(creds, "20260920T000000Z", host, path)
	if other == got {
		t.Fatal("signature does not vary with the date")
	}

	// A session token becomes a signed header.
	tok, err := signRequest(Creds{AccessKey: "AKIATEST", SecretKey: "secretkey", Token: "TOK"}, amzDate, host, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tok, "SignedHeaders=content-type;host;x-amz-content-sha256;x-amz-date;x-amz-security-token,") {
		t.Fatalf("token not signed: %s", tok)
	}
}

func TestRetryable(t *testing.T) {
	ctx := context.Background()
	if retryable(ctx, &s3Err{status: 403}, 0) {
		t.Error("403 must not retry")
	}
	if retryable(ctx, &s3Err{status: 404}, 0) {
		t.Error("404 must not retry")
	}
	if !retryable(ctx, &s3Err{status: 429}, 0) {
		t.Error("429 must retry")
	}
	if !retryable(ctx, &s3Err{status: 500}, 0) {
		t.Error("500 must retry")
	}
	if !retryable(ctx, &s3Err{status: 503}, 2) {
		t.Error("503 must retry until the attempt cap")
	}
	if retryable(ctx, &s3Err{status: 500}, 3) {
		t.Error("attempt cap must stop retries")
	}
	if !retryable(ctx, errors.New("dial tcp: i/o timeout"), 1) {
		t.Error("transport errors must retry")
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if retryable(cctx, &s3Err{status: 500}, 0) {
		t.Error("a cancelled context must stop retries")
	}
}

func TestS3Error(t *testing.T) {
	resp := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(
		`<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`))}
	err := s3Error(resp)
	if err.Error() != "s3: AccessDenied (status 403): denied" {
		t.Fatalf("xml error: %v", err)
	}

	resp = &http.Response{StatusCode: 500, Body: io.NopCloser(strings.NewReader("plain failure"))}
	err = s3Error(resp)
	if !strings.Contains(err.Error(), "plain failure") || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("plain error: %v", err)
	}
}

func TestUploadErrors(t *testing.T) {
	// A missing local file fails before any network I/O.
	err := Upload(context.Background(), &Target{Bucket: "b"}, Creds{},
		[]Object{{Key: "k", Local: "/no/such/file"}})
	if err == nil || !strings.Contains(err.Error(), "put k:") {
		t.Fatalf("local error: %v", err)
	}
	// Nothing to upload is a no-op.
	if err := Upload(context.Background(), &Target{Bucket: "b"}, Creds{}, nil); err != nil {
		t.Fatal(err)
	}
}
