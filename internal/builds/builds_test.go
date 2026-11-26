package builds

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghtest"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tracked"
)

func TestPollObservesJobsOnce(t *testing.T) {
	m := metrics.New("test")
	jobsCalls := 0
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[{"id":9}]}`),
		"/repos/o/r/actions/runs/9/jobs": func(w http.ResponseWriter, _ *http.Request) {
			jobsCalls++
			_, _ = w.Write([]byte(`{"jobs":[
			 {"name":"build","conclusion":"success","created_at":"2026-11-20T10:00:00Z","started_at":"2026-11-20T10:02:00Z","completed_at":"2026-11-20T10:07:00Z"},
			 {"name":"push","conclusion":"skipped","created_at":"2026-11-20T10:00:00Z","started_at":"2026-11-20T10:07:00Z","completed_at":"2026-11-20T10:07:00Z"},
			 {"name":"scan","conclusion":"failure","created_at":"2026-11-20T10:00:00Z","started_at":"2026-11-20T10:01:00Z","completed_at":"2026-11-20T10:03:00Z"}]}`))
		},
	})
	p := New(gh, m, time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	repos := []tracked.Repo{{Owner: "o", Name: "r", PackageWorkflow: "build.yml"}, {Owner: "o", Name: "no-workflow"}}
	for i := 0; i < 2; i++ {
		if err := p.Poll(context.Background(), repos); err != nil {
			t.Fatal(err)
		}
	}
	if jobsCalls != 1 {
		t.Fatalf("jobs fetched %d times, want 1", jobsCalls)
	}
	if n := ghtest.HistCount(t, m, "dora_build_queue_seconds"); n != 2 { // skipped job ignored
		t.Fatalf("queue observations = %d, want 2", n)
	}
	if got := testutil.CollectAndCount(m.BuildDuration); got != 2 { // success and failure series
		t.Fatalf("duration series = %d, want 2", got)
	}
}
