// Package fake is an in-memory store driver for tests and demos.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

type object struct {
	// content is the object's bytes, when the writer provided them
	// (PutContent, Promote). Put leaves it empty, modelling a store that
	// holds content this process never saw — such objects observe with no
	// ContentSHA256, like an S3 object uploaded without a checksum.
	content  string
	digest   string
	metadata map[string]string
}

// Store is an in-memory store.Driver. One instance is shared across all
// classes using the "fake" driver in a process.
type Store struct {
	mu         sync.RWMutex
	objects    map[string]object
	deleteErr  error
	promoteErr error
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
	obs := store.Observation{Exists: true, Digest: o.digest, Metadata: md}
	if o.content != "" {
		obs.ContentSHA256 = contentDigest(o.content)
	}
	return obs, nil
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

// Promote implements store.Promoter: hash the source content, copy it to the
// destination with the caller's metadata plus the content-digest stamp.
func (s *Store) Promote(_ context.Context, req store.PromoteRequest) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.promoteErr != nil {
		return "", s.promoteErr
	}
	src, ok := s.objects[req.SourceKey]
	if !ok {
		return "", fmt.Errorf("promote %q: no such object", req.SourceKey)
	}
	if req.SourceDigest != "" && req.SourceDigest != src.digest {
		return "", fmt.Errorf("promote %q: object changed since observed (digest %s, observed %s)",
			req.SourceKey, src.digest, req.SourceDigest)
	}
	digest := contentDigest(src.content)
	md := make(map[string]string, len(req.Metadata)+1)
	for k, v := range req.Metadata {
		md[k] = v
	}
	md[req.ContentDigestKey] = digest
	s.objects[req.DestKey] = object{content: src.content, digest: src.digest, metadata: md}
	return digest, nil
}

// FailDeletes makes every subsequent Delete return err until called again
// with nil — how tests simulate a store refusing deletion (for example a
// controller missing s3:DeleteObject).
func (s *Store) FailDeletes(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteErr = err
}

// FailPromotions makes every subsequent Promote return err until called again
// with nil — how tests simulate a controller missing copy permissions on the
// canonical prefix.
func (s *Store) FailPromotions(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promoteErr = err
}

// Put simulates a generator uploading an artifact whose bytes the test does
// not care about: the object observes with the given digest and no
// ContentSHA256.
func (s *Store) Put(key, digest string, metadata map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	md := make(map[string]string, len(metadata))
	for k, v := range metadata {
		md[k] = v
	}
	s.objects[key] = object{digest: digest, metadata: md}
}

// PutContent simulates a generator uploading an artifact with actual bytes:
// the store-side digest derives from the content, and observations carry a
// store-computed ContentSHA256 — the S3 checksum analog.
func (s *Store) PutContent(key, content string, metadata map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	md := make(map[string]string, len(metadata))
	for k, v := range metadata {
		md[k] = v
	}
	s.objects[key] = object{content: content, digest: "fake-digest-of:" + content, metadata: md}
}

// Clear removes everything.
func (s *Store) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.objects = map[string]object{}
}

func contentDigest(content string) string {
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])
}
