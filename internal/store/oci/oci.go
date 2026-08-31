// Package oci implements the store driver for OCI registries (ECR, GHCR,
// Harbor, ...). Existence checks read manifests only — image layers are never
// pulled.
package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	ecrlogin "github.com/awslabs/amazon-ecr-credential-helper/ecr-login"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/types"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

// Register wires the oci driver into a registry.
func Register(reg *store.Registry) {
	reg.Register("oci", factory)
}

// keychain resolves registry credentials: ECR repositories via the default
// AWS chain (IRSA/instance role/env), everything else via the ambient docker
// config. Built once per driver instance.
func keychain() authn.Keychain {
	return authn.NewMultiKeychain(
		authn.NewKeychainFromHelper(ecrlogin.NewECRHelper()),
		authn.DefaultKeychain,
	)
}

func factory(_ context.Context, class *artifactsv1.ArtifactClass) (store.Driver, error) {
	cfg := class.Spec.Store.OCI
	if cfg == nil {
		return nil, fmt.Errorf("class %s: store.driver is oci but store.oci is not set", class.Name)
	}
	var opts []name.Option
	if cfg.Insecure {
		opts = append(opts, name.Insecure)
	}
	repo, err := name.NewRepository(cfg.Repository, opts...)
	if err != nil {
		return nil, fmt.Errorf("class %s: invalid repository %q: %w", class.Name, cfg.Repository, err)
	}
	return &driver{
		repo:      repo,
		nameOpts:  opts,
		stampAnno: class.StampMetadataKey(),
		keychain:  keychain(),
	}, nil
}

type driver struct {
	repo      name.Repository
	nameOpts  []name.Option
	stampAnno string
	keychain  authn.Keychain
}

// tag builds the reference for a store key. The key is the tag within the
// class's repository.
func (d *driver) tag(key string) (name.Tag, error) {
	ref, err := name.NewTag(d.repo.Name()+":"+key, d.nameOpts...)
	if err != nil {
		return name.Tag{}, fmt.Errorf("invalid tag %q in %s: %w", key, d.repo.Name(), err)
	}
	return ref, nil
}

func (d *driver) remoteOpts(ctx context.Context) []remote.Option {
	return []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(d.keychain),
	}
}

// Observe fetches the manifest for the tag (not its layers) and reads the
// provenance stamp from the manifest annotations, falling back to the image
// config labels for generators that stamp with a Docker LABEL.
func (d *driver) Observe(ctx context.Context, key string) (store.Observation, error) {
	ref, err := d.tag(key)
	if err != nil {
		return store.Observation{}, err
	}
	desc, err := remote.Get(ref, d.remoteOpts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return store.Observation{}, nil
		}
		return store.Observation{}, fmt.Errorf("get manifest %s: %w", ref, err)
	}

	obs := store.Observation{
		Exists:   true,
		Digest:   desc.Digest.String(),
		Metadata: map[string]string{},
	}

	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(desc.Manifest, &manifest); err != nil {
		return store.Observation{}, fmt.Errorf("parse manifest %s: %w", ref, err)
	}
	for k, v := range manifest.Annotations {
		obs.Metadata[strings.ToLower(k)] = v
	}

	// Fall back to image config labels only when the manifest carries no
	// stamp; this costs one small blob fetch and never touches layers.
	if _, ok := obs.Metadata[d.stampAnno]; !ok && isImageManifest(desc.MediaType) {
		img, err := desc.Image()
		if err != nil {
			return store.Observation{}, fmt.Errorf("read image %s: %w", ref, err)
		}
		cfg, err := img.ConfigFile()
		if err != nil {
			return store.Observation{}, fmt.Errorf("read image config %s: %w", ref, err)
		}
		for k, v := range cfg.Config.Labels {
			lk := strings.ToLower(k)
			if _, exists := obs.Metadata[lk]; !exists {
				obs.Metadata[lk] = v
			}
		}
	}
	return obs, nil
}

// Delete removes the tagged manifest. The registry API requires deletion by
// digest, so the tag is resolved first. Note that registries commonly need
// image deletion enabled (ECR allows it; GHCR/Harbor may not by policy) — a
// rejection surfaces as a store error, never as silent success.
func (d *driver) Delete(ctx context.Context, key string) error {
	ref, err := d.tag(key)
	if err != nil {
		return err
	}
	desc, err := remote.Head(ref, d.remoteOpts(ctx)...)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("head %s: %w", ref, err)
	}
	digestRef, err := name.NewDigest(d.repo.Name()+"@"+desc.Digest.String(), d.nameOpts...)
	if err != nil {
		return fmt.Errorf("build digest reference for %s: %w", ref, err)
	}
	if err := remote.Delete(digestRef, d.remoteOpts(ctx)...); err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %s: %w", digestRef, err)
	}
	// Deleting the manifest is what frees the artifact, and on ECR (and
	// distribution) it also retires every tag pointing at it. Registries that
	// keep tag pointers independently need the tag removed too, so try it and
	// ignore registries that refuse tag deletion.
	if err := remote.Delete(ref, d.remoteOpts(ctx)...); err != nil && !isNotFound(err) && !isUnsupported(err) {
		return fmt.Errorf("delete tag %s: %w", ref, err)
	}
	return nil
}

func isImageManifest(mt types.MediaType) bool {
	return mt == types.OCIManifestSchema1 || mt == types.DockerManifestSchema2
}

// isUnsupported reports registries that decline tag deletion (distribution
// 2.x returns 405; some return 400 UNSUPPORTED).
func isUnsupported(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		if terr.StatusCode == http.StatusMethodNotAllowed || terr.StatusCode == http.StatusBadRequest {
			return true
		}
		for _, e := range terr.Errors {
			if e.Code == transport.UnsupportedErrorCode {
				return true
			}
		}
	}
	return false
}

func isNotFound(err error) bool {
	var terr *transport.Error
	if errors.As(err, &terr) {
		if terr.StatusCode == http.StatusNotFound {
			return true
		}
		for _, e := range terr.Errors {
			switch e.Code {
			case transport.ManifestUnknownErrorCode, transport.NameUnknownErrorCode:
				return true
			}
		}
	}
	return false
}

var _ store.Driver = (*driver)(nil)
