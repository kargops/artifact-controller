package httpstore

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

func classFor(cfg *artifactsv1.HTTPStoreSpec) *artifactsv1.ArtifactClass {
	return &artifactsv1.ArtifactClass{
		Spec: artifactsv1.ArtifactClassSpec{
			Store: artifactsv1.StoreSpec{Driver: "http", HTTP: cfg},
		},
	}
}

func newTestDriver(t *testing.T, cfg *artifactsv1.HTTPStoreSpec, secrets SecretResolver) store.Driver {
	t.Helper()
	d, err := newDriver(context.Background(), classFor(cfg), secrets)
	if err != nil {
		t.Fatalf("newDriver: %v", err)
	}
	return d
}

func TestObserveDefaultExistsUsesStatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/artifacts/present" {
			w.Header().Set("ETag", `"abc123"`)
			w.Header().Set("X-Artifact-Spec-Hash", "sha256:deadbeef")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{Method: "HEAD", URL: srv.URL + "/artifacts/{{ .Key }}"},
		Digest:  `headers['etag']`,
		Stamp:   `headers['x-artifact-spec-hash']`,
	}, nil)

	obs, err := d.Observe(context.Background(), "present")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists {
		t.Fatal("expected Exists")
	}
	if obs.Digest != `"abc123"` {
		t.Errorf("digest = %q", obs.Digest)
	}
	if got := obs.Metadata[artifactsv1.DefaultStampMetadataKey]; got != "sha256:deadbeef" {
		t.Errorf("stamp = %q", got)
	}

	missing, err := d.Observe(context.Background(), "absent")
	if err != nil {
		t.Fatalf("404 must not be an error: %v", err)
	}
	if missing.Exists {
		t.Error("404 must report not-exists")
	}
}

// Stores that answer with a metadata document, rather than headers, are the
// reason the body is exposed to CEL at all.
func TestObserveReadsJSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"checksum":{"sha256":"f00d"},"attributes":{"specHash":"sha256:cafe"}}]}`))
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{Method: "GET", URL: srv.URL + "/search?name={{ .Key }}"},
		Exists:  `code == 200 && json.items.size() > 0`,
		Digest:  `json.items[0].checksum.sha256`,
		Stamp:   `json.items[0].attributes.specHash`,
	}, nil)

	obs, err := d.Observe(context.Background(), "thing")
	if err != nil {
		t.Fatal(err)
	}
	if !obs.Exists || obs.Digest != "f00d" {
		t.Fatalf("obs = %+v", obs)
	}
	if got := obs.Metadata[artifactsv1.DefaultStampMetadataKey]; got != "sha256:cafe" {
		t.Errorf("stamp = %q", got)
	}
}

// An empty result set must read as absent, not as an error: that is the normal
// state before the first build.
func TestExistsFalseOnEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{Method: "GET", URL: srv.URL + "/search?name={{ .Key }}"},
		Exists:  `code == 200 && json.items.size() > 0`,
	}, nil)

	obs, err := d.Observe(context.Background(), "thing")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Exists {
		t.Error("empty result must be absent")
	}
}

func TestAuthSchemes(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	secrets := func(_ context.Context, name string) (map[string][]byte, error) {
		// Trailing newline is the classic result of echoing into a secret.
		return map[string][]byte{
			"token":    []byte("s3cr3t\n"),
			"username": []byte("alice"),
			"password": []byte("hunter2"),
		}, nil
	}

	for _, tc := range []struct {
		name string
		auth *artifactsv1.HTTPAuthSpec
		want string
		hdr  string
	}{
		{
			name: "bearer",
			auth: &artifactsv1.HTTPAuthSpec{Type: artifactsv1.HTTPAuthBearer, SecretRef: &meta.LocalObjectReference{Name: "s"}},
			hdr:  "Authorization",
			want: "Bearer s3cr3t",
		},
		{
			name: "basic",
			auth: &artifactsv1.HTTPAuthSpec{Type: artifactsv1.HTTPAuthBasic, SecretRef: &meta.LocalObjectReference{Name: "s"}},
			hdr:  "Authorization",
			want: "Basic " + base64.StdEncoding.EncodeToString([]byte("alice:hunter2")),
		},
		{
			name: "custom header",
			auth: &artifactsv1.HTTPAuthSpec{Type: artifactsv1.HTTPAuthHeader, HeaderName: "X-JFrog-Art-Api", SecretRef: &meta.LocalObjectReference{Name: "s"}},
			hdr:  "X-JFrog-Art-Api",
			want: "s3cr3t",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
				Observe: artifactsv1.HTTPRequestSpec{Method: "HEAD", URL: srv.URL + "/{{ .Key }}"},
				Auth:    tc.auth,
			}, secrets)
			if _, err := d.Observe(context.Background(), "x"); err != nil {
				t.Fatal(err)
			}
			if v := got.Get(tc.hdr); v != tc.want {
				t.Errorf("%s = %q, want %q", tc.hdr, v, tc.want)
			}
		})
	}
}

func TestDeleteIsIdempotentAndRequiresConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound) // already gone
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	noDelete := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{URL: srv.URL + "/{{ .Key }}"},
	}, nil)
	if err := noDelete.Delete(context.Background(), "x"); err == nil {
		t.Error("delete without configuration must fail loudly, not silently succeed")
	}

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{URL: srv.URL + "/{{ .Key }}"},
		Delete:  &artifactsv1.HTTPRequestSpec{Method: "DELETE", URL: srv.URL + "/{{ .Key }}"},
	}, nil)
	if err := d.Delete(context.Background(), "x"); err != nil {
		t.Errorf("404 on delete must be success: %v", err)
	}
}

// A store answering with a large body must not pull it all into memory.
func TestBodyIsBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		big := make([]byte, 512<<10)
		for i := range big {
			big[i] = 'a'
		}
		_, _ = w.Write(big)
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{Method: "GET", URL: srv.URL + "/{{ .Key }}"},
		Digest:  `string(body.size())`,
	}, nil)

	obs, err := d.Observe(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	if obs.Digest != "65536" {
		t.Errorf("body should be truncated to 64KiB, got size %s", obs.Digest)
	}
}

// Token caching matters: at a one-minute interval across many Artifacts, a
// fresh token per observation would hammer the identity provider, which
// rate-limits token endpoints harder than resource APIs.
func TestClientCredentialsExchangesAndCachesTheToken(t *testing.T) {
	var tokenCalls int
	var seenAuth string
	var seenForm url.Values

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth2/token":
			tokenCalls++
			_ = r.ParseForm()
			seenForm = r.PostForm
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"tok-abc","token_type":"Bearer","expires_in":3600}`))
		default:
			seenAuth = r.Header.Get("Authorization")
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	secrets := func(_ context.Context, _ string) (map[string][]byte, error) {
		return map[string][]byte{"clientId": []byte("app-1"), "clientSecret": []byte("shh")}, nil
	}
	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{Method: "GET", URL: srv.URL + "/users/{{ .Key }}"},
		Auth: &artifactsv1.HTTPAuthSpec{
			Type:      artifactsv1.HTTPAuthClientCredentials,
			TokenURL:  srv.URL + "/oauth2/token",
			Scopes:    []string{"https://graph.microsoft.com/.default"},
			SecretRef: &meta.LocalObjectReference{Name: "graph"},
		},
	}, secrets)

	for i := 0; i < 3; i++ {
		if _, err := d.Observe(context.Background(), "someone@example.com"); err != nil {
			t.Fatalf("observe %d: %v", i, err)
		}
	}

	if seenAuth != "Bearer tok-abc" {
		t.Errorf("Authorization = %q", seenAuth)
	}
	if tokenCalls != 1 {
		t.Errorf("token endpoint called %d times across 3 observations, want 1 (cached)", tokenCalls)
	}
	if got := seenForm.Get("grant_type"); got != "client_credentials" {
		t.Errorf("grant_type = %q", got)
	}
	if got := seenForm.Get("scope"); got != "https://graph.microsoft.com/.default" {
		t.Errorf("scope = %q", got)
	}
	if seenForm.Get("client_secret") != "shh" {
		t.Error("client secret not sent to the token endpoint")
	}
}

// A token endpoint failure must not leak what it reflected back.
func TestClientCredentialsFailureIsOpaque(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_client","client_id":"app-1","hint":"secret shh is wrong"}`))
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{URL: srv.URL + "/{{ .Key }}"},
		Auth: &artifactsv1.HTTPAuthSpec{
			Type: artifactsv1.HTTPAuthClientCredentials, TokenURL: srv.URL + "/token",
			SecretRef: &meta.LocalObjectReference{Name: "s"},
		},
	}, func(_ context.Context, _ string) (map[string][]byte, error) {
		return map[string][]byte{"clientId": []byte("app-1"), "clientSecret": []byte("shh")}, nil
	})

	_, err := d.Observe(context.Background(), "x")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "shh") || strings.Contains(err.Error(), "app-1") {
		t.Errorf("error echoes the token endpoint body: %v", err)
	}
}

// sigv4 signs rather than attaching a static credential, so it needs no
// secret — but it does need to know what the signature is scoped to.
func TestSigV4RequiresRegionAndService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := newTestDriver(t, &artifactsv1.HTTPStoreSpec{
		Observe: artifactsv1.HTTPRequestSpec{URL: srv.URL + "/{{ .Key }}"},
		Auth:    &artifactsv1.HTTPAuthSpec{Type: artifactsv1.HTTPAuthSigV4},
	}, nil)

	_, err := d.Observe(context.Background(), "x")
	if err == nil || !strings.Contains(err.Error(), "region") {
		t.Fatalf("want a clear error naming region, got %v", err)
	}
}
