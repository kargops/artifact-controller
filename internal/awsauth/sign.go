// Package awsauth signs requests with the controller's ambient AWS identity.
// It exists so drivers can reach AWS APIs over plain HTTP without pulling in
// a service SDK: the EC2 SDK alone costs ~2.2GB of build memory, which is a
// poor trade for three API calls.
package awsauth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// EmptyBodySHA256 is the payload hash for a request with no body.
const EmptyBodySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// Sign applies SigV4 using credentials from the default AWS chain — Pod
// Identity, IRSA, environment. Nothing is read from a Kubernetes secret,
// which is the point of those mechanisms.
func Sign(ctx context.Context, req *http.Request, region, service string) error {
	if region == "" {
		return fmt.Errorf("sigv4 needs a region")
	}
	if service == "" {
		return fmt.Errorf("sigv4 needs a service name")
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load aws config: %w", err)
	}
	creds, err := cfg.Credentials.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("retrieve aws credentials: %w", err)
	}

	// The signature covers a hash of the body, so it has to be read and put
	// back for the transport to send.
	payloadHash := EmptyBodySHA256
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return fmt.Errorf("read body for signing: %w", err)
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.ContentLength = int64(len(body))
		sum := sha256.Sum256(body)
		payloadHash = hex.EncodeToString(sum[:])
	}

	if err := v4.NewSigner().SignHTTP(ctx, creds, req, payloadHash, service, region, time.Now()); err != nil {
		return fmt.Errorf("sign request: %w", err)
	}
	return nil
}
