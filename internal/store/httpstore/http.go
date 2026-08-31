// Package httpstore implements a store driver configured entirely from the
// ArtifactClass: request templates for observing and deleting, CEL over the
// response to decide existence and read provenance, and a small closed set of
// auth schemes. It exists so a new store does not require a new Go driver.
package httpstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cel.dev/cel-go/cel"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

// maxBody bounds how much of a response is read into memory and exposed to
// CEL. Stores that answer with metadata (Nexus, Artifactory) return small JSON;
// anything larger is an artifact body we have no business downloading.
const maxBody = 64 << 10

// Register wires the http driver into a registry. Credentials are resolved
// through the supplied SecretResolver, so this package never reads Kubernetes
// objects itself.
func Register(reg *store.Registry, secrets SecretResolver) {
	reg.Register("http", func(ctx context.Context, class *artifactsv1.ArtifactClass) (store.Driver, error) {
		return newDriver(ctx, class, secrets)
	})
}

// SecretResolver returns the data of a secret in the controller's namespace.
type SecretResolver func(ctx context.Context, name string) (map[string][]byte, error)

type driver struct {
	cfg     *artifactsv1.HTTPStoreSpec
	client  *http.Client
	secrets SecretResolver
	env     *cel.Env
	tokens  tokenCache
}

func newDriver(_ context.Context, class *artifactsv1.ArtifactClass, secrets SecretResolver) (store.Driver, error) {
	cfg := class.Spec.Store.HTTP
	if cfg == nil {
		return nil, fmt.Errorf("class %s: store.driver is http but store.http is not set", class.Name)
	}
	if cfg.Observe.URL == "" {
		return nil, fmt.Errorf("class %s: store.http.observe.url is required", class.Name)
	}
	// Variables mirror what a response actually offers, so expressions read
	// the way an operator would describe them.
	env, err := cel.NewEnv(
		cel.Variable("code", cel.IntType),
		cel.Variable("headers", cel.MapType(cel.StringType, cel.StringType)),
		cel.Variable("body", cel.StringType),
		cel.Variable("json", cel.DynType),
	)
	if err != nil {
		return nil, fmt.Errorf("class %s: build CEL env: %w", class.Name, err)
	}
	timeout := 30 * time.Second
	if cfg.Timeout != nil && cfg.Timeout.Duration > 0 {
		timeout = cfg.Timeout.Duration
	}
	return &driver{
		cfg:     cfg,
		client:  &http.Client{Timeout: timeout},
		secrets: secrets,
		env:     env,
	}, nil
}

func (d *driver) Observe(ctx context.Context, key string) (store.Observation, error) {
	resp, err := d.do(ctx, d.cfg.Observe, key)
	if err != nil {
		return store.Observation{}, err
	}

	exists, err := d.evalBool(d.cfg.Exists, resp, defaultExists)
	if err != nil {
		return store.Observation{}, fmt.Errorf("evaluate exists: %w", err)
	}
	if !exists {
		return store.Observation{}, nil
	}

	obs := store.Observation{Exists: true, Metadata: map[string]string{}}
	if d.cfg.Digest != "" {
		v, err := d.evalString(d.cfg.Digest, resp)
		if err != nil {
			return store.Observation{}, fmt.Errorf("evaluate digest: %w", err)
		}
		obs.Digest = v
	}
	if d.cfg.Stamp != "" {
		v, err := d.evalString(d.cfg.Stamp, resp)
		if err != nil {
			return store.Observation{}, fmt.Errorf("evaluate stamp: %w", err)
		}
		// The controller looks the stamp up by the class's metadata key, so
		// publish it under that name whatever its source was.
		obs.Metadata[strings.ToLower(d.stampKey())] = v
	}
	return obs, nil
}

func (d *driver) Delete(ctx context.Context, key string) error {
	if d.cfg.Delete == nil {
		return fmt.Errorf("store.http.delete is not configured; this class cannot delete artifacts")
	}
	resp, err := d.do(ctx, *d.cfg.Delete, key)
	if err != nil {
		return err
	}
	// 404 means already gone, which is success for an idempotent delete.
	if resp.code == http.StatusNotFound || (resp.code >= 200 && resp.code < 300) {
		return nil
	}
	return fmt.Errorf("delete returned %d", resp.code)
}

func (d *driver) stampKey() string {
	if d.cfg.StampKey != "" {
		return d.cfg.StampKey
	}
	return artifactsv1.DefaultStampMetadataKey
}

// response is what CEL expressions see.
type response struct {
	code    int
	headers map[string]string
	body    string
	json    interface{}
}

func (d *driver) do(ctx context.Context, req artifactsv1.HTTPRequestSpec, key string) (*response, error) {
	url, err := renderTemplate(req.URL, key)
	if err != nil {
		return nil, err
	}
	method := req.Method
	if method == "" {
		method = http.MethodHead
	}
	httpReq, err := http.NewRequestWithContext(ctx, method, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	if err := d.authorize(ctx, httpReq); err != nil {
		return nil, err
	}

	httpResp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer httpResp.Body.Close()

	out := &response{code: httpResp.StatusCode, headers: map[string]string{}}
	for k := range httpResp.Header {
		// Lowercase so expressions do not depend on server capitalisation.
		out.headers[strings.ToLower(k)] = httpResp.Header.Get(k)
	}
	body, err := io.ReadAll(io.LimitReader(httpResp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	out.body = string(body)
	out.json = parseJSON(out.body)
	return out, nil
}

func defaultExists(r *response) bool { return r.code >= 200 && r.code < 300 }
