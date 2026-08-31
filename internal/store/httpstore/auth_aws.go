package httpstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
)

// emptyBodySHA256 is the payload hash SigV4 requires for a request with no
// body, which every observation and delete here has.
const emptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// signSigV4 signs the request with the controller's ambient AWS identity.
// Nothing is read from a secret: the whole point of Pod Identity and IRSA is
// that no long-lived credential exists to put in one.
func (d *driver) signSigV4(ctx context.Context, req *http.Request, auth *artifactsv1.HTTPAuthSpec) error {
	if auth.Region == "" {
		return fmt.Errorf("auth type sigv4 requires store.http.auth.region")
	}
	if auth.Service == "" {
		return fmt.Errorf("auth type sigv4 requires store.http.auth.service (e.g. s3, execute-api, es)")
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(auth.Region))
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve aws credentials: %w", err)
	}

	// The signature covers a hash of the body, so it has to be read and
	// restored. Requests here are bodyless, but be correct rather than lucky.
	payloadHash := emptyBodySHA256
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("read body for signing: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	if err := v4.NewSigner().SignHTTP(ctx, creds, req, payloadHash, auth.Service, auth.Region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	return nil
}
