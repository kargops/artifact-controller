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
