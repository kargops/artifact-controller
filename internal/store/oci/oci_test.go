package oci

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
)

const stamp = "sha256:72ddd979f2af866f8356b3ffe6e43584f7233bef60021f9882d40cc26f8bc776"

// testDriver spins up an in-memory registry and returns a driver pointed at
// a repository in it.
func testDriver(t *testing.T) (*driver, string) {
	t.Helper()
	srv := httptest.NewServer(registry.New())
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	repoName := u.Host + "/game-clients"
	class := &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{
				Driver: "oci",
				OCI:    &artifactsv1.OCIStoreSpec{Repository: repoName, Insecure: true},
			},
		},
	}
	d, err := factory(context.Background(), class)
	if err != nil {
		t.Fatal(err)
	}
	drv := d.(*driver)
	// The in-memory registry is unauthenticated; skip the ECR helper, which
	// would otherwise try to resolve credentials for a localhost host.
	drv.keychain = authn.NewMultiKeychain()
	return drv, repoName
}

func pushImage(t *testing.T, repoName, tag string, annotations map[string]string) v1.Hash {
	t.Helper()
	img, err := random.Image(64, 1)
	if err != nil {
		t.Fatal(err)
	}
	if annotations != nil {
		img = mutate.Annotations(img, annotations).(v1.Image)
	}
	ref, err := name.NewTag(repoName+":"+tag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	dig, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return dig
}

func TestObserveMissing(t *testing.T) {
	d, _ := testDriver(t)
	obs, err := d.Observe(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("missing tag must not error: %v", err)
	}
	if obs.Exists {
		t.Fatal("expected Exists=false for absent tag")
	}
}

func TestObserveReadsDigestAndStampAnnotation(t *testing.T) {
	d, repoName := testDriver(t)
	want := pushImage(t, repoName, "v1", map[string]string{
		artifactsv1.DefaultOCIStampAnnotation: stamp,
	})

	obs, err := d.Observe(context.Background(), "v1")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists {
		t.Fatal("expected Exists=true")
	}
	if obs.Digest != want.String() {
		t.Fatalf("digest = %q, want %q", obs.Digest, want.String())
	}
	if got := obs.Metadata[artifactsv1.DefaultOCIStampAnnotation]; got != stamp {
		t.Fatalf("stamp = %q, want %q", got, stamp)
	}
}

func TestObserveUnstampedImageHasNoStamp(t *testing.T) {
	d, repoName := testDriver(t)
	pushImage(t, repoName, "v2", nil)

	obs, err := d.Observe(context.Background(), "v2")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists {
		t.Fatal("expected Exists=true")
	}
	// No stamp means "adoptable" to the reconciler, not a conflict.
	if got := obs.Metadata[artifactsv1.DefaultOCIStampAnnotation]; got != "" {
		t.Fatalf("unexpected stamp %q", got)
	}
}

func TestDeleteRemovesTagAndIsIdempotent(t *testing.T) {
	d, repoName := testDriver(t)
	pushImage(t, repoName, "v3", map[string]string{artifactsv1.DefaultOCIStampAnnotation: stamp})

	ctx := context.Background()
	if err := d.Delete(ctx, "v3"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	obs, err := d.Observe(ctx, "v3")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Exists {
		t.Fatal("tag still present after delete")
	}
	// Deleting an absent artifact is not an error.
	if err := d.Delete(ctx, "v3"); err != nil {
		t.Fatalf("second delete must be a no-op, got %v", err)
	}
}

func TestInvalidTagIsRejected(t *testing.T) {
	d, _ := testDriver(t)
	// ':' is legal in a content address but not in an OCI tag — this is why
	// oci classes default to the .SpecHex key template.
	_, err := d.Observe(context.Background(), stamp)
	if err == nil || !strings.Contains(err.Error(), "invalid tag") {
		t.Fatalf("expected invalid tag error, got %v", err)
	}
}
