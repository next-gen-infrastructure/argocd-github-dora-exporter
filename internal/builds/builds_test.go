package builds

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
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
	p := New(gh, m, slog.New(slog.DiscardHandler), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
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

func TestPollSkipsPullRequestRuns(t *testing.T) {
	m := metrics.New("test")
	jobsFor := map[string]int{}
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[
			{"id":1,"event":"pull_request"},{"id":2,"event":"pull_request_target"},{"id":3,"event":"push"}]}`),
		"/repos/o/r/actions/runs/1/jobs": func(w http.ResponseWriter, _ *http.Request) { jobsFor["1"]++; _, _ = w.Write([]byte(`{"jobs":[]}`)) },
		"/repos/o/r/actions/runs/2/jobs": func(w http.ResponseWriter, _ *http.Request) { jobsFor["2"]++; _, _ = w.Write([]byte(`{"jobs":[]}`)) },
		"/repos/o/r/actions/runs/3/jobs": func(w http.ResponseWriter, _ *http.Request) {
			jobsFor["3"]++
			_, _ = w.Write([]byte(`{"jobs":[{"name":"b","conclusion":"success","created_at":"2026-11-20T10:00:00Z","started_at":"2026-11-20T10:01:00Z","completed_at":"2026-11-20T10:03:00Z"}]}`))
		},
	})
	p := New(gh, m, slog.New(slog.DiscardHandler), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	if err := p.Poll(context.Background(), []tracked.Repo{{Owner: "o", Name: "r", PackageWorkflow: "build.yml"}}); err != nil {
		t.Fatal(err)
	}
	if jobsFor["1"] != 0 || jobsFor["2"] != 0 || jobsFor["3"] != 1 {
		t.Fatalf("jobs fetched per run = %v, want only the push run", jobsFor)
	}
	if n := ghtest.HistCount(t, m, "dora_build_duration_seconds"); n != 1 {
		t.Fatalf("build observations = %d, want 1", n)
	}
}

func TestPollMissingWorkflowIsNotAnErrorAndWarnsOnce(t *testing.T) {
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/actions/workflows/build.yml/runs": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
		"/repos/o/ok/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[{"id":3,"event":"push"}]}`),
		"/repos/o/ok/actions/runs/3/jobs": ghtest.JSON(`{"jobs":[{"name":"b","conclusion":"success",
			"created_at":"2026-11-20T10:00:00Z","started_at":"2026-11-20T10:01:00Z","completed_at":"2026-11-20T10:03:00Z"}]}`),
	})
	var logs bytes.Buffer
	p := New(gh, m, slog.New(slog.NewTextHandler(&logs, nil)), time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC))
	repos := []tracked.Repo{{Owner: "o", Name: "r", PackageWorkflow: "build.yml"}, {Owner: "o", Name: "ok", PackageWorkflow: "build.yml"}}
	for i := 0; i < 3; i++ {
		if err := p.Poll(context.Background(), repos); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if n := strings.Count(logs.String(), "package workflow not found"); n != 1 {
		t.Fatalf("warned %d times, want once:\n%s", n, logs.String())
	}
	if n := ghtest.HistCount(t, m, "dora_build_duration_seconds"); n != 1 {
		t.Fatalf("the repo with the workflow was not observed: %d", n)
	}
}
