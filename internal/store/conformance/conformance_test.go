// Package conformance runs one shared contract suite against every store
// driver. The fake driver anchors the contract on every test run; the s3 and
// oci drivers run against the real store software in containers (MinIO, CNCF
// distribution) and skip cleanly where Docker is unavailable — the same
// posture as hand-written mocks would be, except a real Nexus-style semantic
// trap (a store that answers 200 where the driver expects 404) cannot hide in
// a mock we wrote ourselves.
package conformance

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
	"github.com/kargops/artifact-controller/internal/store/fake"
	ocistore "github.com/kargops/artifact-controller/internal/store/oci"
	s3store "github.com/kargops/artifact-controller/internal/store/s3"
)

// Image pins follow the repo convention: never "latest". registry:3 is the
// same image the quickstart runs in-cluster.
const (
	minioImage    = "minio/minio:RELEASE.2025-07-23T15-54-02Z"
	registryImage = "registry:3"
)

// runContract is the driver contract. seed simulates a generator upload:
// distinct content values must produce distinct store-side digests.
func runContract(t *testing.T, d store.Driver, stampKey string, seed func(t *testing.T, key, content, stamp string)) {
	t.Helper()
	ctx := context.Background()
	key := fmt.Sprintf("conformance-%s-%d", strings.ToLower(t.Name()[strings.LastIndex(t.Name(), "/")+1:]), time.Now().UnixNano())
	const stamp = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	obs, err := d.Observe(ctx, key)
	if err != nil {
		t.Fatalf("observe absent key: %v", err)
	}
	if obs.Exists {
		t.Fatalf("absent key %q reported Exists=true", key)
	}

	if err := d.Delete(ctx, key); err != nil {
		t.Fatalf("deleting an absent object must not be an error, got: %v", err)
	}

	seed(t, key, "content-one", stamp)
	obs, err = d.Observe(ctx, key)
	if err != nil {
		t.Fatalf("observe seeded key: %v", err)
	}
	if !obs.Exists {
		t.Fatalf("seeded key %q reported Exists=false", key)
	}
	if obs.Digest == "" {
		t.Fatalf("seeded key %q carries no digest — drift detection would be blind", key)
	}
	if got := obs.Metadata[stampKey]; got != stamp {
		t.Fatalf("provenance stamp not surfaced: metadata[%q] = %q, want %q (all metadata: %v)", stampKey, got, stamp, obs.Metadata)
	}
	first := obs.Digest

	seed(t, key, "content-two", stamp)
	obs, err = d.Observe(ctx, key)
	if err != nil {
		t.Fatalf("observe after overwrite: %v", err)
	}
	if obs.Digest == first {
		t.Fatalf("digest did not change on content change (%q) — drift detection would be blind", first)
	}

	if err := d.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	obs, err = d.Observe(ctx, key)
	if err != nil {
		t.Fatalf("observe after delete: %v", err)
	}
	if obs.Exists {
		t.Fatalf("key %q still exists after Delete", key)
	}

	if err := d.Delete(ctx, key); err != nil {
		t.Fatalf("second delete must not be an error, got: %v", err)
	}
}

func TestFakeDriverConformance(t *testing.T) {
	s := fake.New()
	runContract(t, s, artifactsv1.DefaultStampMetadataKey, func(t *testing.T, key, content, stamp string) {
		s.Put(key, "digest-of-"+content, map[string]string{artifactsv1.DefaultStampMetadataKey: stamp})
	})
}

func TestS3DriverConformanceAgainstMinIO(t *testing.T) {
	requireDocker(t)
	endpoint := startContainer(t, minioImage, "9000",
		[]string{"-e", "MINIO_ROOT_USER=conformance", "-e", "MINIO_ROOT_PASSWORD=conformance-secret"},
		[]string{"server", "/data"},
		func(base string) string { return base + "/minio/health/live" })

	// The driver reads credentials from the default AWS chain; feed it the
	// container's static ones and keep it away from IMDS.
	t.Setenv("AWS_ACCESS_KEY_ID", "conformance")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "conformance-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	class := &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{Store: artifactsv1.StoreSpec{
			Driver: "s3",
			S3: &artifactsv1.S3StoreSpec{
				Bucket:       "conformance",
				Region:       "us-east-1",
				Endpoint:     endpoint,
				UsePathStyle: true,
			},
		}},
	}
	class.Name = "conformance-s3"
	reg := store.NewRegistry()
	s3store.Register(reg)
	driver, err := reg.DriverFor(context.Background(), class)
	if err != nil {
		t.Fatalf("build s3 driver: %v", err)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion("us-east-1"))
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	if _, err := client.CreateBucket(context.Background(), &awss3.CreateBucketInput{Bucket: aws.String("conformance")}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	runContract(t, driver, artifactsv1.DefaultStampMetadataKey, func(t *testing.T, key, content, stamp string) {
		t.Helper()
		_, err := client.PutObject(context.Background(), &awss3.PutObjectInput{
			Bucket:   aws.String("conformance"),
			Key:      aws.String(key),
			Body:     strings.NewReader(content),
			Metadata: map[string]string{artifactsv1.DefaultStampMetadataKey: stamp},
		})
		if err != nil {
			t.Fatalf("seed s3 object: %v", err)
		}
	})
}

func TestOCIDriverConformanceAgainstRegistry(t *testing.T) {
	requireDocker(t)
	endpoint := startContainer(t, registryImage, "5000",
		[]string{"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true"},
		nil,
		func(base string) string { return base + "/v2/" })
	host := strings.TrimPrefix(endpoint, "http://")

	class := &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{Store: artifactsv1.StoreSpec{
			Driver: "oci",
			OCI: &artifactsv1.OCIStoreSpec{
				Repository: host + "/conformance",
				Insecure:   true,
			},
		}},
	}
	class.Name = "conformance-oci"
	reg := store.NewRegistry()
	ocistore.Register(reg)
	driver, err := reg.DriverFor(context.Background(), class)
	if err != nil {
		t.Fatalf("build oci driver: %v", err)
	}

	runContract(t, driver, artifactsv1.DefaultOCIStampAnnotation, func(t *testing.T, key, content, stamp string) {
		t.Helper()
		// random.Image gives distinct layer content per call, which is what
		// makes the manifest digest change on "overwrite" — an image's bytes
		// are its layers, so content is unused beyond that.
		_ = content
		img, err := random.Image(64, 1)
		if err != nil {
			t.Fatalf("build random image: %v", err)
		}
		annotated, ok := mutate.Annotations(img, map[string]string{artifactsv1.DefaultOCIStampAnnotation: stamp}).(v1.Image)
		if !ok {
			t.Fatalf("mutate.Annotations did not return a v1.Image")
		}
		tag, err := name.NewTag(host+"/conformance:"+key, name.Insecure)
		if err != nil {
			t.Fatalf("build tag: %v", err)
		}
		if err := remote.Write(tag, annotated); err != nil {
			t.Fatalf("push image: %v", err)
		}
	})
}

// --- container plumbing ---

func requireDocker(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: skipping container-backed conformance")
	}
	if os.Getenv("SKIP_CONFORMANCE") == "1" {
		t.Skip("SKIP_CONFORMANCE=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, "docker", "info").Run(); err != nil {
		t.Skipf("docker unavailable, skipping container-backed conformance: %v", err)
	}
}

// startContainer runs image detached with the container port published on a
// random loopback port, waits for readyPath to answer 200, and returns
// "http://127.0.0.1:<port>". The container is removed via t.Cleanup.
func startContainer(t *testing.T, image, port string, env []string, cmd []string, readyPath func(base string) string) string {
	t.Helper()
	args := append([]string{"run", "-d", "--rm", "-p", "127.0.0.1:0:" + port}, env...)
	args = append(args, image)
	args = append(args, cmd...)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run %s: %v", image, err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	mapped, err := exec.Command("docker", "port", id, port+"/tcp").Output()
	if err != nil {
		t.Fatalf("docker port %s: %v", id, err)
	}
	// "0.0.0.0:49153" or "127.0.0.1:49153", possibly multiple lines.
	hostPort := strings.TrimSpace(strings.Split(string(mapped), "\n")[0])
	hostPort = hostPort[strings.LastIndex(hostPort, ":")+1:]
	base := "http://127.0.0.1:" + hostPort

	deadline := time.Now().Add(60 * time.Second)
	url := readyPath(base)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return base
			}
		}
		if time.Now().After(deadline) {
			logs, _ := exec.Command("docker", "logs", id).CombinedOutput()
			t.Fatalf("%s not ready after 60s at %s (last error: %v)\ncontainer logs:\n%s", image, url, err, logs)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
