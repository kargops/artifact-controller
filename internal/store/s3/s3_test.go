package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestContentSHA256Normalization(t *testing.T) {
	sum := sha256.Sum256([]byte("content"))
	b64 := base64.StdEncoding.EncodeToString(sum[:])
	want := fmt.Sprintf("sha256:%x", sum)

	cases := []struct {
		name string
		b64  string
		typ  types.ChecksumType
		want string
	}{
		{"full object checksum", b64, types.ChecksumTypeFullObject, want},
		{"untyped full checksum", b64, "", want},
		{"absent", "", "", ""},
		// A composite checksum is a checksum of part checksums, not of the
		// content; treating it as a content hash would fail verification of
		// every multipart object.
		{"composite by suffix", b64 + "-4", "", ""},
		{"composite by type", b64, types.ChecksumTypeComposite, ""},
		{"not base64", "%%%", "", ""},
		{"wrong length", base64.StdEncoding.EncodeToString([]byte("short")), "", ""},
	}
	for _, tc := range cases {
		if got := contentSHA256(tc.b64, tc.typ); got != tc.want {
			t.Errorf("%s: contentSHA256(%q, %q) = %q, want %q", tc.name, tc.b64, tc.typ, got, tc.want)
		}
	}
}

// TestCopySourceEscaping pins the CopySource encoding: segments are escaped,
// path separators are not — S3 expects "bucket/key" with the key's own
// slashes intact.
func TestCopySourceEscaping(t *testing.T) {
	got := (&url.URL{Path: "my-bucket/incoming/sha256:abc def"}).EscapedPath()
	want := "my-bucket/incoming/sha256:abc%20def"
	if got != want {
		t.Fatalf("escaped copy source = %q, want %q", got, want)
	}
}

func TestETagIfMatchQuotesStrippedObservationDigest(t *testing.T) {
	cases := []struct {
		digest string
		want   string
	}{
		{"etag:abc123", `"abc123"`},
		{`etag:"abc123"`, `"abc123"`},
		{"etag:abc-2", `"abc-2"`},
		{"sha256-b64:abcd", ""},
		{"etag:", ""},
		{"", ""},
	}
	for _, tc := range cases {
		got := ""
		if p := etagIfMatch(tc.digest); p != nil {
			got = *p
		}
		if got != tc.want {
			t.Errorf("etagIfMatch(%q) = %q, want %q", tc.digest, got, tc.want)
		}
	}
}

func TestApplyObjectHeadersCopiesSystemHeaders(t *testing.T) {
	exp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	cp := &awss3.CopyObjectInput{}
	applyObjectHeaders(cp, &awss3.GetObjectOutput{
		ContentType:        aws.String("application/gzip"),
		ContentEncoding:    aws.String("gzip"),
		ContentDisposition: aws.String("attachment"),
		ContentLanguage:    aws.String("en"),
		CacheControl:       aws.String("public"),
		Expires:            &exp,
	})
	if aws.ToString(cp.ContentType) != "application/gzip" ||
		aws.ToString(cp.ContentEncoding) != "gzip" ||
		aws.ToString(cp.ContentDisposition) != "attachment" ||
		aws.ToString(cp.ContentLanguage) != "en" ||
		aws.ToString(cp.CacheControl) != "public" ||
		cp.Expires == nil || !cp.Expires.Equal(exp) {
		t.Fatalf("system headers not copied: %#v", cp)
	}
}

// A store that rejects ChecksumMode fails HeadObject for both present and
// missing keys. The retry without the header must surface a missing key as
// absence, not as the original rejection.
func TestObserveChecksumRetryReportsMissingKey(t *testing.T) {
	var sawChecksum bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-amz-checksum-mode") != "" {
			sawChecksum = true
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `<Error><Code>InvalidRequest</Code><Message>checksum mode unsupported</Message></Error>`)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `<Error><Code>NotFound</Code><Message>Not Found</Message></Error>`)
	}))
	t.Cleanup(srv.Close)

	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")),
	)
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := awss3.NewFromConfig(cfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(srv.URL)
		o.UsePathStyle = true
	})
	d := &driver{client: client, bucket: "bucket", checksums: true}

	obs, err := d.Observe(ctx, "missing-key")
	if err != nil {
		t.Fatalf("Observe missing key: %v", err)
	}
	if obs.Exists || obs.Digest != "" || obs.ContentSHA256 != "" {
		t.Fatalf("Observe missing key = %#v, want absence", obs)
	}
	if !sawChecksum {
		t.Fatal("first HeadObject did not send checksum mode")
	}
}
