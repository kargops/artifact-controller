package httpstore

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
)

// authorize applies the class's auth scheme to a request. The set is closed:
// each entry either sends a credential verbatim or runs a standard, widely
// specified exchange. Anything needing bespoke logic belongs in a driver.
func (d *driver) authorize(ctx context.Context, req *http.Request) error {
	auth := d.cfg.Auth
	if auth == nil || auth.Type == "" || auth.Type == artifactsv1.HTTPAuthNone {
		return nil
	}
	// SigV4 signs the request itself rather than attaching a static header, so
	// it takes credentials from the ambient AWS chain (Pod Identity, IRSA) and
	// needs no secret.
	if auth.Type == artifactsv1.HTTPAuthSigV4 {
		return d.signSigV4(ctx, req, auth)
	}
	if auth.SecretRef == nil {
		return fmt.Errorf("auth type %q requires store.http.auth.secretRef", auth.Type)
	}
	if d.secrets == nil {
		return fmt.Errorf("auth type %q configured but no secret resolver is available", auth.Type)
	}
	data, err := d.secrets(ctx, auth.SecretRef.Name)
	if err != nil {
		return fmt.Errorf("read secret %q: %w", auth.SecretRef.Name, err)
	}

	switch auth.Type {
	case artifactsv1.HTTPAuthBearer:
		token, err := requireKey(data, keyOr(auth.TokenKey, "token"))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	case artifactsv1.HTTPAuthBasic:
		user, err := requireKey(data, keyOr(auth.UsernameKey, "username"))
		if err != nil {
			return err
		}
		pass, err := requireKey(data, keyOr(auth.PasswordKey, "password"))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(user+":"+pass)))
	case artifactsv1.HTTPAuthClientCredentials:
		token, err := d.clientCredentialsToken(ctx, auth, data)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
	case artifactsv1.HTTPAuthHeader:
		name := keyOr(auth.HeaderName, "Authorization")
		value, err := requireKey(data, keyOr(auth.TokenKey, "token"))
		if err != nil {
			return err
		}
		req.Header.Set(name, value)
	default:
		return fmt.Errorf("unsupported auth type %q", auth.Type)
	}
	return nil
}

func keyOr(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}

func requireKey(data map[string][]byte, key string) (string, error) {
	v, ok := data[key]
	if !ok || len(v) == 0 {
		return "", fmt.Errorf("secret has no non-empty key %q", key)
	}
	// Trailing newlines are the classic result of `echo` into a secret and
	// would corrupt a header value.
	return strings.TrimRight(string(v), "\r\n"), nil
}
