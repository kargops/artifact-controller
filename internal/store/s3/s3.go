// Package s3 implements the store driver for S3-compatible object stores.
// Existence checks are metadata-only (HeadObject) — artifact content is only
// ever read during promotion, when it is hashed.
package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

// copyObjectSizeLimit is the S3 single-request CopyObject ceiling. Promotion
// of larger objects needs multipart copy, which this driver does not do yet.
const copyObjectSizeLimit = 5 * 1024 * 1024 * 1024

// Register wires the s3 driver into a registry.
func Register(reg *store.Registry) {
	reg.Register("s3", factory)
}

func factory(ctx context.Context, class *artifactsv1.ArtifactClass) (store.Driver, error) {
	cfg := class.Spec.Store.S3
	if cfg == nil {
		return nil, fmt.Errorf("class %s: store.driver is s3 but store.s3 is not set", class.Name)
	}
	var loadOpts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("class %s: load aws config: %w", class.Name, err)
	}
	client := awss3.NewFromConfig(awsCfg, func(o *awss3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		o.UsePathStyle = cfg.UsePathStyle
	})
	return &driver{client: client, bucket: cfg.Bucket}, nil
}

type driver struct {
	client *awss3.Client
	bucket string
}

func (d *driver) Observe(ctx context.Context, key string) (store.Observation, error) {
	head, err := d.client.HeadObject(ctx, &awss3.HeadObjectInput{
		Bucket:       aws.String(d.bucket),
		Key:          aws.String(key),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		if isNotFound(err) {
			return store.Observation{}, nil
		}
		return store.Observation{}, fmt.Errorf("head s3://%s/%s: %w", d.bucket, key, err)
	}
	obs := store.Observation{Exists: true, Metadata: map[string]string{}}
	// ETag before checksum: earlier versions of this driver never requested
	// ChecksumMode, so every digest recorded by existing estates is an ETag —
	// preferring the (stronger) checksum here would flip the digest form of
	// checksummed objects on upgrade and fire one false drift per artifact.
	switch {
	case head.ETag != nil && *head.ETag != "":
		obs.Digest = "etag:" + strings.Trim(*head.ETag, `"`)
	case head.ChecksumSHA256 != nil && *head.ChecksumSHA256 != "":
		obs.Digest = "sha256-b64:" + *head.ChecksumSHA256
	}
	obs.ContentSHA256 = contentSHA256(aws.ToString(head.ChecksumSHA256), head.ChecksumType)
	for k, v := range head.Metadata {
		obs.Metadata[strings.ToLower(k)] = v
	}
	return obs, nil
}

func (d *driver) Delete(ctx context.Context, key string) error {
	_, err := d.client.DeleteObject(ctx, &awss3.DeleteObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete s3://%s/%s: %w", d.bucket, key, err)
	}
	return nil
}

// Promote hashes the object at SourceKey, then server-side-copies it onto
// DestKey with the caller's metadata plus the content-digest stamp. The bytes
// hashed are guaranteed to be the bytes copied: the GetObject and the
// CopyObject are both pinned to the same ETag, so an object swapped in
// between fails the copy instead of inheriting the verification.
func (d *driver) Promote(ctx context.Context, req store.PromoteRequest) (string, error) {
	get := &awss3.GetObjectInput{
		Bucket: aws.String(d.bucket),
		Key:    aws.String(req.SourceKey),
	}
	if etag := strings.TrimPrefix(req.SourceDigest, "etag:"); etag != req.SourceDigest && etag != "" {
		get.IfMatch = aws.String(etag)
	}
	obj, err := d.client.GetObject(ctx, get)
	if err != nil {
		return "", fmt.Errorf("get s3://%s/%s for promotion: %w", d.bucket, req.SourceKey, err)
	}
	defer obj.Body.Close()
	if size := aws.ToInt64(obj.ContentLength); size > copyObjectSizeLimit {
		return "", fmt.Errorf("object s3://%s/%s is %d bytes; promotion supports up to %d (single CopyObject)",
			d.bucket, req.SourceKey, size, copyObjectSizeLimit)
	}
	h := sha256.New()
	if _, err := io.Copy(h, obj.Body); err != nil {
		return "", fmt.Errorf("hash s3://%s/%s: %w", d.bucket, req.SourceKey, err)
	}
	sum := h.Sum(nil)
	digest := "sha256:" + hex.EncodeToString(sum)

	metadata := make(map[string]string, len(req.Metadata)+1)
	for k, v := range req.Metadata {
		metadata[k] = v
	}
	metadata[req.ContentDigestKey] = digest

	cp := &awss3.CopyObjectInput{
		Bucket:            aws.String(d.bucket),
		Key:               aws.String(req.DestKey),
		CopySource:        aws.String((&url.URL{Path: d.bucket + "/" + req.SourceKey}).EscapedPath()),
		MetadataDirective: types.MetadataDirectiveReplace,
		Metadata:          metadata,
		// Have the store compute and record its own sha256 of the destination,
		// so later observations can verify the content-digest stamp against a
		// value no writer chose.
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
	}
	if obj.ETag != nil && *obj.ETag != "" {
		cp.CopySourceIfMatch = obj.ETag
	}
	out, err := d.client.CopyObject(ctx, cp)
	if err != nil {
		return "", fmt.Errorf("promote s3://%s/%s to %q: %w", d.bucket, req.SourceKey, req.DestKey, err)
	}
	// The store hashed the same bytes we did; disagreement means corruption in
	// flight and must not become a verified artifact.
	if out.CopyObjectResult != nil {
		want := base64.StdEncoding.EncodeToString(sum)
		if got := aws.ToString(out.CopyObjectResult.ChecksumSHA256); got != "" && got != want {
			return "", fmt.Errorf("promote s3://%s/%s to %q: store computed sha256 %s, this controller computed %s",
				d.bucket, req.SourceKey, req.DestKey, got, want)
		}
	}
	return digest, nil
}

// contentSHA256 normalizes an S3 sha256 checksum (base64) to "sha256:<hex>".
// Composite (multipart) checksums are checksums-of-checksums, not content
// hashes, and are reported as unavailable.
func contentSHA256(b64 string, typ types.ChecksumType) string {
	if b64 == "" || strings.Contains(b64, "-") || typ == types.ChecksumTypeComposite {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) != sha256.Size {
		return ""
	}
	return "sha256:" + hex.EncodeToString(raw)
}

func isNotFound(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}
