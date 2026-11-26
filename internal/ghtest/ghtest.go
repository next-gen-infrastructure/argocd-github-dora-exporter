// Package ghtest provides a fake GitHub API and a client wired to it, for tests.
package ghtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

// New starts a fake API. routes maps a URL path to a handler; the installation token endpoint
// is built in. The returned client talks to the fake and records into m.
func New(t *testing.T, m *metrics.Metrics, routes map[string]http.HandlerFunc) *ghclient.Client {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemKey := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/app/installations/2/access_tokens" {
			_, _ = w.Write([]byte(`{"token":"t","expires_at":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`))
			return
		}
		if h, ok := routes[r.URL.Path]; ok {
			h(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	c, err := ghclient.New("1", "2", string(pemKey), m)
	if err != nil {
		t.Fatal(err)
	}
	c.SetBaseURL(srv.URL)
	return c
}

// JSON returns a handler that serves body.
func JSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
}

// HistCount returns the total observation count of the named histogram across all its series.
func HistCount(t *testing.T, m *metrics.Metrics, name string) uint64 {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var n uint64
	for _, f := range fams {
		if f.GetName() != name {
			continue
		}
		for _, mm := range f.GetMetric() {
			n += mm.GetHistogram().GetSampleCount()
		}
	}
	return n
}
