package leadtime

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/deploy"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghtest"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

func TestHandleObservesLeadTimePerCommitOnceAndRollout(t *testing.T) {
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/compare/aaa1111...bbb2222": ghtest.JSON(`{"commits":[
			{"sha":"c1","parents":[{"sha":"x"}],"commit":{"committer":{"date":"2026-11-20T08:00:00Z"}}},
			{"sha":"c2","parents":[{"sha":"c1"}],"commit":{"committer":{"date":"2026-11-20T09:00:00Z"}}},
			{"sha":"merge","parents":[{"sha":"c2"},{"sha":"y"}],"commit":{"committer":{"date":"2026-11-20T09:30:00Z"}}}]}`),
		"/repos/o/r/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[
			{"id":1,"head_sha":"merge","created_at":"2026-11-20T09:35:00Z","updated_at":"2026-11-20T09:45:00Z"}]}`),
	})
	w := New(gh, m, slog.New(slog.DiscardHandler))
	at := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	d := deploy.Deployed{
		App:     argocd.App{Name: "ns-app", Owner: "o", Repo: "r", Ref: "bbb2222", PackageWorkflow: "build.yml"},
		PrevRef: "aaa1111", At: at,
	}
	if err := w.Handle(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if err := w.Handle(context.Background(), d); err != nil { // same commits again: no new observations
		t.Fatal(err)
	}
	if got := testutil.CollectAndCount(m.LeadTime); got != 1 {
		t.Fatalf("lead time series = %d", got)
	}
	// c1: 2h, c2: 1h, merge commit skipped; second Handle adds nothing.
	if n := ghtest.HistCount(t, m, "dora_lead_time_seconds"); n != 2 {
		t.Fatalf("lead time observations = %d, want 2", n)
	}
	if got := testutil.CollectAndCount(m.Rollout); got != 1 {
		t.Fatalf("rollout series = %d", got)
	}
}

func TestHandleObservesImageUpdateFromGitOpsCommit(t *testing.T) {
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/compare/aaa1111...bbb2222": ghtest.JSON(`{"commits":[
			{"sha":"c1","parents":[{"sha":"x"}],"commit":{"committer":{"date":"2026-11-20T08:00:00Z"}}}]}`),
		"/repos/o/r/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[
			{"id":2,"head_sha":"c1","created_at":"2026-11-20T09:50:00Z","updated_at":"2026-11-20T09:55:00Z"},
			{"id":1,"head_sha":"c1","created_at":"2026-11-20T09:35:00Z","updated_at":"2026-11-20T09:45:00Z"}]}`),
		// Newest first: the next write-back (mentions our tag as the OLD one), an unrelated edit,
		// the write-back of our tag, and an older tag.
		"/repos/o/gitops/commits": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			if q.Get("sha") != "dev" || q.Get("path") != "ns/app/values-dev.yaml" {
				http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[
			 {"sha":"4","commit":{"message":"build: automatic update of app\n\nupdates image o/r tag 'tag-bbb2222' to 'tag-next'","committer":{"date":"2026-11-20T09:59:00Z"}}},
			 {"sha":"3","commit":{"message":"chore: tune resources","committer":{"date":"2026-11-20T09:58:00Z"}}},
			 {"sha":"2","commit":{"message":"build: automatic update of app\n\nupdates image o/r tag 'aaa1111' to 'tag-bbb2222'","committer":{"date":"2026-11-20T09:50:00Z"}}},
			 {"sha":"1","commit":{"message":"build: automatic update of app\n\nupdates image o/r tag 'x' to 'aaa1111'","committer":{"date":"2026-11-19T09:50:00Z"}}}]`))
		},
	})
	w := New(gh, m, slog.New(slog.DiscardHandler))
	d := deploy.Deployed{
		App: argocd.App{Name: "ns-app", Owner: "o", Repo: "r", Ref: "bbb2222", Tag: "tag-bbb2222", PackageWorkflow: "build.yml",
			GitOpsOwner: "o", GitOpsRepo: "gitops", WriteBackFile: "ns/app/values-dev.yaml", WriteBackBranch: "dev"},
		PrevRef: "aaa1111", At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
	}
	if err := w.Handle(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	// Image built by the first successful run (09:45; a later re-run finished 09:55), tag written back 09:50 => 300s (not the later commits that only mention the tag as the old one).
	if n := ghtest.HistCount(t, m, "dora_image_update_seconds"); n != 1 {
		t.Fatalf("image update observations = %d, want 1", n)
	}
	if got := histSum(t, m, "dora_image_update_seconds"); got != 300 {
		t.Fatalf("image update = %vs, want 300", got)
	}

	// No write-back file known: skipped, not zero.
	d.App.WriteBackFile = ""
	if err := w.Handle(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if n := ghtest.HistCount(t, m, "dora_image_update_seconds"); n != 1 {
		t.Fatalf("image update observed without a write-back file: %d", n)
	}
}

func histSum(t *testing.T, m *metrics.Metrics, name string) float64 {
	t.Helper()
	fams, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() == name {
			for _, mm := range f.GetMetric() {
				sum += mm.GetHistogram().GetSampleSum()
			}
		}
	}
	return sum
}

func TestHandleRolloutIgnoresPullRequestRunsAndMissingWorkflow(t *testing.T) {
	compare := ghtest.JSON(`{"commits":[
		{"sha":"c1","parents":[{"sha":"x"}],"commit":{"committer":{"date":"2026-11-20T08:00:00Z"}}}]}`)
	d := deploy.Deployed{
		App:     argocd.App{Name: "ns-app", Owner: "o", Repo: "r", Ref: "bbb2222", PackageWorkflow: "build.yml"},
		PrevRef: "aaa1111", At: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC),
	}

	// Only a pull request run exists for the commit: no rollout, no error.
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/compare/aaa1111...bbb2222": compare,
		"/repos/o/r/actions/workflows/build.yml/runs": ghtest.JSON(`{"workflow_runs":[
			{"id":1,"event":"pull_request","head_sha":"c1","updated_at":"2026-11-20T09:45:00Z"}]}`),
	})
	if err := New(gh, m, slog.New(slog.DiscardHandler)).Handle(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if n := ghtest.HistCount(t, m, "dora_rollout_seconds"); n != 0 {
		t.Fatalf("rollout measured from a pull request run: %d", n)
	}

	// The workflow does not exist: lead time is still observed and nothing fails.
	m = metrics.New("test")
	gh = ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/compare/aaa1111...bbb2222": compare,
		"/repos/o/r/actions/workflows/build.yml/runs": func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		},
	})
	if err := New(gh, m, slog.New(slog.DiscardHandler)).Handle(context.Background(), d); err != nil {
		t.Fatalf("missing workflow must not fail the deployment: %v", err)
	}
	if n := ghtest.HistCount(t, m, "dora_lead_time_seconds"); n != 1 {
		t.Fatalf("lead time not observed: %d", n)
	}
}
