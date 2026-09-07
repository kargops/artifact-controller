package s3

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"testing"

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
