package repomanager

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

func newFor(t *testing.T, f flavour, baseURL string, secretData map[string][]byte) store.Driver {
	t.Helper()
	class := &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{
				Driver: "artifactory",
				RepoManager: &artifactsv1.RepoManagerStoreSpec{
					BaseURL:    baseURL,
					Repository: "builds",
				},
			},
		},
	}
	var secrets SecretResolver
	if secretData != nil {
		class.Spec.Store.RepoManager.SecretRef = &meta.LocalObjectReference{Name: "creds"}
		secrets = func(context.Context, string) (map[string][]byte, error) { return secretData, nil }
	}
	d, err := newDriver(context.Background(), class, secrets, f)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	return d
}

func TestArtifactoryReadsChecksumAndProperties(t *testing.T) {
	var seenAuth, seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth, seenPath = r.Header.Get("X-JFrog-Art-Api"), r.URL.Path
		if r.URL.Path != "/api/storage/builds/client-abc.tar.gz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{
		  "checksums": {"sha256": "f00dfeed"},
		  "properties": {"artifact-spec-hash": ["sha256:cafe"], "build.number": ["42"]}
		}`))
	}))
	defer srv.Close()

	d := newFor(t, flavourArtifactory, srv.URL, map[string][]byte{"apiKey": []byte("k3y\n")})
	obs, err := d.Observe(context.Background(), "client-abc.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists {
		t.Fatal("expected Exists")
	}
	if obs.Digest != "sha256:f00dfeed" {
		t.Errorf("digest = %q", obs.Digest)
	}
	if got := obs.Metadata["artifact-spec-hash"]; got != "sha256:cafe" {
		t.Errorf("stamp = %q", got)
	}
	if seenAuth != "k3y" {
		t.Errorf("api key not sent (or newline not trimmed): %q", seenAuth)
	}
	if seenPath != "/api/storage/builds/client-abc.tar.gz" {
		t.Errorf("path = %q", seenPath)
	}

	missing, err := d.Observe(context.Background(), "absent.tar.gz")
	if err != nil {
		t.Fatalf("404 must not error: %v", err)
	}
	if missing.Exists {
		t.Error("404 must report absent")
	}
}

// The trap this driver exists to remove: Nexus answers 200 with an empty item
// list for an artifact that is not there, so a status-code check alone reports
// everything as present.
func TestNexusEmptyResultIsAbsentNotPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") == "present.tar.gz" {
			_, _ = w.Write([]byte(`{"items":[{"id":"abc123","path":"present.tar.gz","checksum":{"sha256":"beef"}}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	d := newFor(t, flavourNexus, srv.URL, nil)

	missing, err := d.Observe(context.Background(), "absent.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Exists {
		t.Fatal("200 with an empty item list must report absent")
	}

	obs, err := d.Observe(context.Background(), "present.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists || obs.Digest != "sha256:beef" {
		t.Fatalf("obs = %+v", obs)
	}
}

// Nexus deletes by component id, which only the search knows — the reason a
// hand-written class cannot express deletion at all.
func TestNexusDeleteResolvesTheAssetId(t *testing.T) {
	var deletedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete:
			deletedPath = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"items":[{"id":"asset-id-7","path":"x.tar.gz"}]}`))
		}
	}))
	defer srv.Close()

	d := newFor(t, flavourNexus, srv.URL, nil)
	if err := d.Delete(context.Background(), "x.tar.gz"); err != nil {
		t.Fatal(err)
	}
	if deletedPath != "/service/rest/v1/assets/asset-id-7" {
		t.Errorf("deleted %q, want the id resolved from the search", deletedPath)
	}
}

func TestAuthSchemeFollowsTheSecretContents(t *testing.T) {
	var seen http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	for _, tc := range []struct {
		name string
		data map[string][]byte
		hdr  string
		want string
	}{
		{"api key", map[string][]byte{"apiKey": []byte("k")}, "X-JFrog-Art-Api", "k"},
		{"bearer", map[string][]byte{"token": []byte("t")}, "Authorization", "Bearer t"},
		{"basic", map[string][]byte{"username": []byte("u"), "password": []byte("p")}, "Authorization",
			"Basic " + base64.StdEncoding.EncodeToString([]byte("u:p"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFor(t, flavourNexus, srv.URL, tc.data)
			if _, err := d.Observe(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			if got := seen.Get(tc.hdr); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.hdr, got, tc.want)
			}
		})
	}
}

func TestUnusableSecretFailsAtConstruction(t *testing.T) {
	class := &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{
				Driver: "nexus",
				RepoManager: &artifactsv1.RepoManagerStoreSpec{
					BaseURL: "https://nexus.internal", Repository: "builds",
					SecretRef: &meta.LocalObjectReference{Name: "creds"},
				},
			},
		},
	}
	_, err := newDriver(context.Background(), class,
		func(context.Context, string) (map[string][]byte, error) {
			return map[string][]byte{"nonsense": []byte("x")}, nil
		}, flavourNexus)
	if err == nil {
		t.Fatal("a secret with no usable credential must fail loudly at setup")
	}
}
