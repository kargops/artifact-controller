// Package fake is an in-memory store driver for tests and demos.
package fake

import (
	"context"
	"sync"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

type object struct {
	digest   string
	metadata map[string]string
}

// Store is an in-memory store.Driver. One instance is shared across all
// classes using the "fake" driver in a process.
type Store struct {
	mu        sync.RWMutex
	objects   map[string]object
	deleteErr error
}

func New() *Store {
	return &Store{objects: map[string]object{}}
}

// Register wires the shared instance into a registry under driver name "fake".
func Register(reg *store.Registry, s *Store) {
	reg.Register("fake", func(_ context.Context, _ *artifactsv1.ArtifactClass) (store.Driver, error) {
		return s, nil
	})
}

func (s *Store) Observe(_ context.Context, key string) (store.Observation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.objects[key]
	if !ok {
		return store.Observation{}, nil
	}
	md := make(map[string]string, len(o.metadata))
	for k, v := range o.metadata {
		md[k] = v
	}
	return store.Observation{Exists: true, Digest: o.digest, Metadata: md}, nil
}

func (s *Store) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.objects, key)
	return nil
}

// FailDeletes makes every subsequent Delete return err until called again
// with nil — how tests simulate a store refusing deletion (for example a
// controller missing s3:DeleteObject).
func (s *Store) FailDeletes(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

// Put simulates a generator uploading an artifact.
func (s *Store) Put(key, digest string, metadata map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	md := make(map[string]string, len(metadata))
	for k, v := range metadata {
		md[k] = v
	}
	s.objects[key] = object{digest: digest, metadata: md}
}

// Clear removes everything.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = map[string]object{}
}
