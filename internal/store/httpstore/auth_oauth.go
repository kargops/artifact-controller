package httpstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	artifactsv1 "github.com/kargops/artifact-controller/api/v1alpha1"
)

// tokenCache holds one client-credentials token per driver. Without it every
// observation would mint a fresh token — at a one-minute interval across many
// Artifacts that is a lot of round trips, and identity providers rate-limit
// the token endpoint harder than the resource API.
type tokenCache struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// earlyExpiry retires a token before the provider does, so a request is never
// signed with a credential that expires in flight.
const earlyExpiry = 60 * time.Second

func (c *tokenCache) get() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expiresAt) {
		return c.token, true
	}
	return "", false
}

func (c *tokenCache) put(token string, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = token
	c.expiresAt = time.Now().Add(ttl - earlyExpiry)
}

// clientCredentialsToken performs the OAuth2 client-credentials exchange —
// the flow Microsoft Graph, Keycloak and most machine-to-machine APIs use,
// where the credential in the secret is exchanged for a short-lived token
// rather than being sent to the resource itself.
func (d *driver) clientCredentialsToken(ctx context.Context, auth *artifactsv1.HTTPAuthSpec, data map[string][]byte) (string, error) {
	if tok, ok := d.tokens.get(); ok {
		return tok, nil
	}
	if auth.TokenURL == "" {
		return "", fmt.Errorf("auth type clientCredentials requires store.http.auth.tokenURL")
	}
	clientID, err := requireKey(data, keyOr(auth.ClientIDKey, "clientId"))
	if err != nil {
		return "", err
	}
	clientSecret, err := requireKey(data, keyOr(auth.ClientSecretKey, "clientSecret"))
	if err != nil {
		return "", err
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	}
	if len(auth.Scopes) > 0 {
		form.Set("scope", strings.Join(auth.Scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := d.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Deliberately does not echo the body: token endpoints reflect the
		// client_id and sometimes more on failure.
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token response carried no access_token")
	}

	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= earlyExpiry {
		// A provider that reports no or a very short lifetime gets used once
		// rather than cached into the past.
		ttl = 2 * earlyExpiry
	}
	d.tokens.put(out.AccessToken, ttl)
	return out.AccessToken, nil
}
