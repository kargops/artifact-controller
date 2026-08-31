// Package s3 implements the store driver for S3-compatible object stores.
// Existence checks are metadata-only (HeadObject) — artifact content is never
// downloaded.
package s3

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
	"github.com/kargops/artifact-controller/internal/store"
)

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
		Bucket: aws.String(d.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return store.Observation{}, nil
		}
		return store.Observation{}, fmt.Errorf("head s3://%s/%s: %w", d.bucket, key, err)
	}
	obs := store.Observation{Exists: true, Metadata: map[string]string{}}
	switch {
	case head.ChecksumSHA256 != nil && *head.ChecksumSHA256 != "":
		obs.Digest = "sha256-b64:" + *head.ChecksumSHA256
	case head.ETag != nil:
		obs.Digest = "etag:" + strings.Trim(*head.ETag, `"`)
	}
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
