// Package ghclient is a small GitHub App client: JWT auth, installation token cache,
// REST and GraphQL calls, and request/rate-limit metrics.
package ghclient

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

const apiBase = "https://api.github.com"

// Client talks to the GitHub API as an App installation.
type Client struct {
	base       string
	hc         *http.Client
	appID      string
	installID  string
	key        *rsa.PrivateKey
	now        func() time.Time
	m          *metrics.Metrics
	static     string // static token; replaces the App installation token
	mu         sync.Mutex
	token      string
	tokenUntil time.Time
}

// New creates a client from the App credentials (PEM private key).
func New(appID, installationID, privateKeyPEM string, m *metrics.Metrics) (*Client, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(privateKeyPEM))
	if err != nil {
		return nil, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	return &Client{base: apiBase, hc: &http.Client{Timeout: 30 * time.Second},
		appID: appID, installID: installationID, key: key, now: time.Now, m: m}, nil
}

// NewWithToken creates a client that authenticates with a static token (for example a
// read-only fine-grained personal access token) instead of a GitHub App.
func NewWithToken(token string, m *metrics.Metrics) *Client {
	return &Client{base: apiBase, hc: &http.Client{Timeout: 30 * time.Second}, static: token, now: time.Now, m: m}
}

// SetBaseURL points the client at another API root (tests, GitHub Enterprise).
func (c *Client) SetBaseURL(u string) { c.base = u }

// REST performs an authenticated GET of path (with query) and decodes the JSON into out.
func (c *Client) REST(ctx context.Context, path string, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, "rest", http.MethodGet, c.base+path, tok, nil, out)
}

// GraphQL runs a query and decodes its data into out. GraphQL errors are returned as errors.
func (c *Client) GraphQL(ctx context.Context, query string, vars map[string]any, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	var resp struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	body := map[string]any{"query": query, "variables": vars}
	if err := c.do(ctx, "graphql", http.MethodPost, c.base+"/graphql", tok, body, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("github graphql: %s", resp.Errors[0].Message)
	}
	return json.Unmarshal(resp.Data, out)
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	if c.static != "" {
		return c.static, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.tokenUntil.Add(-time.Minute)) {
		return c.token, nil
	}
	appJWT, err := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    c.appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}).SignedString(c.key)
	if err != nil {
		return "", err
	}
	var resp struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	url := fmt.Sprintf("%s/app/installations/%s/access_tokens", c.base, c.installID)
	if err := c.do(ctx, "rest", http.MethodPost, url, appJWT, nil, &resp); err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	c.token, c.tokenUntil = resp.Token, resp.ExpiresAt
	return c.token, nil
}

func (c *Client) do(ctx context.Context, api, method, url, bearer string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		c.m.GitHubRequests.WithLabelValues(api, "error").Inc()
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	c.m.GitHubRequests.WithLabelValues(api, strconv.Itoa(resp.StatusCode)).Inc()
	if v, err := strconv.ParseFloat(resp.Header.Get("X-RateLimit-Remaining"), 64); err == nil {
		c.m.RateLimitRemaining.WithLabelValues(api).Set(v)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github %s %s: %s: %.200s", method, req.URL.Path, resp.Status, data)
	}
	return json.Unmarshal(data, out)
}
