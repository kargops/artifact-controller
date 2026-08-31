// Package repomanager implements drivers for the two repository managers that
// turn up everywhere: Artifactory and Nexus. Both are reachable through the
// generic http driver, but encoding them here means a class says where the
// artifact lives and nothing about how to ask — and the awkward parts (Nexus
// answering 200 with an empty result set, Artifactory's separate properties
// endpoint, deleting by component id) are handled once rather than by every
// author who copies a snippet.
package repomanager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

const maxBody = 64 << 10

// SecretResolver reads a credential secret from the controller's namespace.
type SecretResolver func(ctx context.Context, name string) (map[string][]byte, error)

// Register wires both drivers into a registry.
func Register(reg *store.Registry, secrets SecretResolver) {
	reg.Register("artifactory", func(ctx context.Context, c *artifactsv1.ArtifactClass) (store.Driver, error) {
		return newDriver(ctx, c, secrets, flavourArtifactory)
	})
	reg.Register("nexus", func(ctx context.Context, c *artifactsv1.ArtifactClass) (store.Driver, error) {
		return newDriver(ctx, c, secrets, flavourNexus)
	})
}

type flavour int

const (
	flavourArtifactory flavour = iota
	flavourNexus
)

type driver struct {
	flavour  flavour
	cfg      *artifactsv1.RepoManagerStoreSpec
	client   *http.Client
	auth     func(*http.Request)
	stampKey string
}

func newDriver(ctx context.Context, class *artifactsv1.ArtifactClass, secrets SecretResolver, f flavour) (store.Driver, error) {
	cfg := class.Spec.Store.RepoManager
	if cfg == nil {
		return nil, fmt.Errorf("class %s: store.driver is %s but store.repoManager is not set",
			class.Name, class.Spec.Store.Driver)
	}
	if cfg.BaseURL == "" || cfg.Repository == "" {
		return nil, fmt.Errorf("class %s: store.repoManager needs both baseURL and repository", class.Name)
	}
	d := &driver{
		flavour:  f,
		cfg:      cfg,
		client:   &http.Client{Timeout: timeoutOr(cfg.Timeout)},
		stampKey: class.StampMetadataKey(),
		auth:     func(*http.Request) {},
	}
	if cfg.SecretRef != nil {
		if secrets == nil {
			return nil, fmt.Errorf("class %s: secretRef set but no secret resolver is available", class.Name)
		}
		data, err := secrets(ctx, cfg.SecretRef.Name)
		if err != nil {
			return nil, fmt.Errorf("class %s: read secret %q: %w", class.Name, cfg.SecretRef.Name, err)
		}
		auth, err := authFrom(data)
		if err != nil {
			return nil, fmt.Errorf("class %s: %w", class.Name, err)
		}
		d.auth = auth
	}
	return d, nil
}

func timeoutOr(d *metav1.Duration) time.Duration {
	if d != nil && d.Duration > 0 {
		return d.Duration
	}
	return 30 * time.Second
}

// authFrom picks the scheme from what the secret actually carries: an API key
// for Artifactory, a bearer token, or a username/password pair. Both products
// accept all three depending on deployment, so the secret decides rather than
// the class carrying a redundant type field.
func authFrom(data map[string][]byte) (func(*http.Request), error) {
	get := func(k string) string { return strings.TrimRight(string(data[k]), "\r\n") }
	switch {
	case get("apiKey") != "":
		key := get("apiKey")
		return func(r *http.Request) { r.Header.Set("X-JFrog-Art-Api", key) }, nil
	case get("token") != "":
		token := get("token")
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+token) }, nil
	case get("username") != "" && get("password") != "":
		enc := base64.StdEncoding.EncodeToString([]byte(get("username") + ":" + get("password")))
		return func(r *http.Request) { r.Header.Set("Authorization", "Basic "+enc) }, nil
	default:
		return nil, fmt.Errorf("secret carries none of apiKey, token, or username+password")
	}
}

func (d *driver) Observe(ctx context.Context, key string) (store.Observation, error) {
	if d.flavour == flavourArtifactory {
		return d.observeArtifactory(ctx, key)
	}
	return d.observeNexus(ctx, key)
}

func (d *driver) Delete(ctx context.Context, key string) error {
	if d.flavour == flavourArtifactory {
		return d.deleteArtifactory(ctx, key)
	}
	return d.deleteNexus(ctx, key)
}

func (d *driver) do(ctx context.Context, method, rawURL string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	d.auth(req)

	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, rawURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, body, nil
}

func (d *driver) base() string { return strings.TrimRight(d.cfg.BaseURL, "/") }

// --- Artifactory -----------------------------------------------------------

// observeArtifactory reads the storage API, which returns checksums and, with
// properties requested, the provenance in one call.
func (d *driver) observeArtifactory(ctx context.Context, key string) (store.Observation, error) {
	u := fmt.Sprintf("%s/api/storage/%s/%s?properties", d.base(), d.cfg.Repository, key)
	code, body, err := d.do(ctx, http.MethodGet, u)
	if err != nil {
		return store.Observation{}, err
	}
	if code == http.StatusNotFound {
		return store.Observation{}, nil
	}
	if code != http.StatusOK {
		return store.Observation{}, fmt.Errorf("artifactory returned %d for %s", code, key)
	}

	var out struct {
		Checksums struct {
			SHA256 string `json:"sha256"`
		} `json:"checksums"`
		Properties map[string][]string `json:"properties"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return store.Observation{}, fmt.Errorf("decode artifactory response: %w", err)
	}

	obs := store.Observation{Exists: true, Metadata: map[string]string{}}
	if out.Checksums.SHA256 != "" {
		obs.Digest = "sha256:" + out.Checksums.SHA256
	}
	// Properties are multi-valued; the first is the one a generator set.
	for k, v := range out.Properties {
		if len(v) > 0 {
			obs.Metadata[strings.ToLower(k)] = v[0]
		}
	}
	return obs, nil
}

func (d *driver) deleteArtifactory(ctx context.Context, key string) error {
	u := fmt.Sprintf("%s/%s/%s", d.base(), d.cfg.Repository, key)
	code, _, err := d.do(ctx, http.MethodDelete, u)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound || (code >= 200 && code < 300) {
		return nil
	}
	return fmt.Errorf("artifactory delete returned %d", code)
}

// --- Nexus -----------------------------------------------------------------

type nexusAssets struct {
	Items []struct {
		ID       string `json:"id"`
		Path     string `json:"path"`
		Checksum struct {
			SHA256 string `json:"sha256"`
		} `json:"checksum"`
	} `json:"items"`
}

// observeNexus searches for the asset. Nexus answers 200 with an empty item
// list for something that does not exist, so the status code alone would
// report every artifact as present — the single most common way to get a
// hand-written Nexus class wrong.
func (d *driver) observeNexus(ctx context.Context, key string) (store.Observation, error) {
	u := fmt.Sprintf("%s/service/rest/v1/search/assets?repository=%s&name=%s",
		d.base(), url.QueryEscape(d.cfg.Repository), url.QueryEscape(key))
	code, body, err := d.do(ctx, http.MethodGet, u)
	if err != nil {
		return store.Observation{}, err
	}
	if code == http.StatusNotFound {
		return store.Observation{}, nil
	}
	if code != http.StatusOK {
		return store.Observation{}, fmt.Errorf("nexus returned %d for %s", code, key)
	}

	var out nexusAssets
	if err := json.Unmarshal(body, &out); err != nil {
		return store.Observation{}, fmt.Errorf("decode nexus response: %w", err)
	}
	if len(out.Items) == 0 {
		return store.Observation{}, nil
	}
	item := out.Items[0]

	obs := store.Observation{Exists: true, Metadata: map[string]string{}}
	if item.Checksum.SHA256 != "" {
		obs.Digest = "sha256:" + item.Checksum.SHA256
	}
	// Nexus carries no arbitrary asset metadata, so there is nowhere for a
	// generator to write a stamp. Provenance therefore rests entirely on the
	// key being content-addressed: an artifact at the hash's path is by
	// construction the artifact that hash describes. Absent stamps are
	// adoptable, which is exactly the intended behaviour here.
	return obs, nil
}

func (d *driver) deleteNexus(ctx context.Context, key string) error {
	// Deletion is by component id, which only the search knows.
	u := fmt.Sprintf("%s/service/rest/v1/search/assets?repository=%s&name=%s",
		d.base(), url.QueryEscape(d.cfg.Repository), url.QueryEscape(key))
	code, body, err := d.do(ctx, http.MethodGet, u)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound {
		return nil
	}
	if code != http.StatusOK {
		return fmt.Errorf("nexus search returned %d", code)
	}
	var out nexusAssets
	if err := json.Unmarshal(body, &out); err != nil {
		return fmt.Errorf("decode nexus response: %w", err)
	}
	if len(out.Items) == 0 {
		return nil
	}

	del := fmt.Sprintf("%s/service/rest/v1/assets/%s", d.base(), url.PathEscape(out.Items[0].ID))
	code, _, err = d.do(ctx, http.MethodDelete, del)
	if err != nil {
		return err
	}
	if code == http.StatusNotFound || (code >= 200 && code < 300) {
		return nil
	}
	return fmt.Errorf("nexus delete returned %d", code)
}

var _ store.Driver = (*driver)(nil)
