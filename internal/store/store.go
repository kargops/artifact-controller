// Package store defines the driver contract for external artifact stores.
// The shape (Observe/Delete against an external system) is adapted from
// Crossplane's ExternalClient pattern.
package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
)

// Observation is the result of checking a store for an artifact.
type Observation struct {
	// Exists reports whether an object is present at the key.
	Exists bool
	// Digest is a driver-specific version identifier of the observed object.
	Digest string
	// ContentSHA256 is the store's own full-object sha256 of the current
	// content ("sha256:<hex>"), when the store exposes one. Unlike Metadata
	// it is computed by the store rather than declared by the writer, which
	// is what makes it usable for content verification. Empty when the store
	// has no checksum for the object (never uploaded with one, composite
	// multipart checksums, stores without checksum support).
	ContentSHA256 string
	// Metadata is the store-side object metadata (normalized to lowercase
	// keys), which carries the generator's provenance stamp.
	Metadata map[string]string
}

// Driver observes and deletes objects in one external store, scoped to one
// ArtifactClass.
type Driver interface {
	Observe(ctx context.Context, key string) (Observation, error)
	// Delete removes the object at key. Deleting an absent object is not an
	// error.
	Delete(ctx context.Context, key string) error
}

// PromoteRequest asks a driver to verify and promote one object.
type PromoteRequest struct {
	// SourceKey is the object to promote; DestKey is where it becomes the
	// canonical artifact. They may be equal, which stamps the object in place
	// (how a pre-promotion object is adopted into the model).
	SourceKey string
	DestKey   string
	// SourceDigest, when set, is the Digest of a prior Observation of the
	// source; promotion must fail if the object has changed since. The
	// caller's provenance checks were made against that version, and a
	// swapped object must not inherit them.
	SourceDigest string
	// Metadata is stamped onto the promoted object, replacing the source
	// object's own metadata: provenance is the controller's statement, not
	// the generator's.
	Metadata map[string]string
	// ContentDigestKey is the metadata key stamped with the content digest
	// the driver computes.
	ContentDigestKey string
}

// Promoter is the optional driver capability behind promotion-enabled
// classes: hash the object at SourceKey, then copy it to DestKey stamped with
// req.Metadata plus the computed content digest under ContentDigestKey —
// guaranteeing the hashed bytes are the copied bytes. Returns the content
// digest ("sha256:<hex>").
type Promoter interface {
	Promote(ctx context.Context, req PromoteRequest) (string, error)
}

// Factory builds a Driver from a class's store configuration.
type Factory func(ctx context.Context, class *artifactsv1.ArtifactClass) (Driver, error)

// Registry maps driver names to factories.
type Registry struct {
	mu        sync.RWMutex
	factories map[string]Factory
}

func NewRegistry() *Registry {
	return &Registry{factories: map[string]Factory{}}
}

func (r *Registry) Register(name string, f Factory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.factories[name] = f
}

// DriverFor resolves the driver for a class. An unregistered driver is a
// terminal-ish configuration error: the caller surfaces it on the Artifact and
// runs no generator, so a misconfigured class never burns pipeline capacity.
func (r *Registry) DriverFor(ctx context.Context, class *artifactsv1.ArtifactClass) (Driver, error) {
	r.mu.RLock()
	f, ok := r.factories[class.Spec.Store.Driver]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no store driver registered for %q (available: %s)",
			class.Spec.Store.Driver, strings.Join(r.registered(), ", "))
	}
	return f(ctx, class)
}

// registered returns the sorted names of all registered drivers.
func (r *Registry) registered() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.factories))
	for name := range r.factories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
