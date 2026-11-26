package ghclient_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghtest"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

func TestCompareHandlesPagesAndCap(t *testing.T) {
	m := metrics.New("test")
	var calls atomic.Int32
	page := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"commits":[`)
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"sha":"s%d","parents":[{"sha":"p"}],"commit":{"committer":{"date":"2026-11-20T10:00:00Z"}}}`, i)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	c := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/compare/a...b": func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Query().Get("page") == "1" {
				w.Header().Set("X-RateLimit-Remaining", "4000")
				_, _ = w.Write([]byte(page(100)))
				return
			}
			_, _ = w.Write([]byte(page(30)))
		},
	})

	got, err := c.Compare(context.Background(), "o", "r", "a", "b", 1000)
	if err != nil || len(got) != 130 || calls.Load() != 2 {
		t.Fatalf("got %d commits, %d calls, err %v", len(got), calls.Load(), err)
	}
	got, _ = c.Compare(context.Background(), "o", "r", "a", "b", 50)
	if len(got) != 50 {
		t.Fatalf("cap not applied: %d", len(got))
	}
	if testutil.ToFloat64(m.GitHubRequests.WithLabelValues("rest", "200")) < 3 {
		t.Fatal("requests not counted")
	}
}

func TestGraphQLErrorsAndTokenReuse(t *testing.T) {
	m := metrics.New("test")
	c := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/graphql": ghtest.JSON(`{"errors":[{"message":"boom"}]}`),
	})
	var out struct{}
	err := c.GraphQL(context.Background(), "query{}", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	_ = c.GraphQL(context.Background(), "query{}", nil, &out)
	// one token request + two graphql requests
	if got := testutil.ToFloat64(m.GitHubRequests.WithLabelValues("rest", "200")); got != 1 {
		t.Fatalf("token requested %v times, want 1", got)
	}
}
