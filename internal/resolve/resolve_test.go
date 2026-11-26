package resolve_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghtest"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/resolve"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func run(path, event, conclusion, start, end string) ghclient.Run {
	return ghclient.Run{Path: path, Event: event, Conclusion: conclusion, RunStartedAt: ts(start), UpdatedAt: ts(end)}
}

func TestPickRun(t *testing.T) {
	wb := ts("2026-11-20T10:00:00Z")
	build := run(".github/workflows/ci.yml", "push", "success", "2026-11-20T09:40:00Z", "2026-11-20T09:59:00Z")
	for name, c := range map[string]struct {
		runs []ghclient.Run
		want string
	}{
		"the build": {[]ghclient.Run{build}, "ci.yml"},
		"ignores PR runs": {[]ghclient.Run{
			run(".github/workflows/pr.yml", "pull_request", "success", "2026-11-20T09:40:00Z", "2026-11-20T09:59:30Z"), build}, "ci.yml"},
		"ignores unsuccessful runs": {[]ghclient.Run{
			run(".github/workflows/x.yml", "push", "cancelled", "2026-11-20T09:40:00Z", "2026-11-20T09:59:50Z"), build}, "ci.yml"},
		"ignores runs that ended long before": {[]ghclient.Run{
			run(".github/workflows/lint.yml", "push", "success", "2026-11-20T08:00:00Z", "2026-11-20T08:05:00Z"), build}, "ci.yml"},
		"ignores runs that started after the write-back": {[]ghclient.Run{
			run(".github/workflows/later.yml", "push", "success", "2026-11-20T10:01:00Z", "2026-11-20T10:09:00Z"), build}, "ci.yml"},
		"a run still finishing after the write-back counts": {[]ghclient.Run{
			run(".github/workflows/ci-dev.yaml", "push", "success", "2026-11-20T09:30:00Z", "2026-11-20T10:01:00Z")}, "ci-dev.yaml"},
		"closest finish wins": {[]ghclient.Run{
			run(".github/workflows/a.yml", "push", "success", "2026-11-20T09:30:00Z", "2026-11-20T09:56:00Z"),
			run(".github/workflows/b.yml", "push", "success", "2026-11-20T09:30:00Z", "2026-11-20T09:59:30Z")}, "b.yml"},
		"none": {[]ghclient.Run{
			run(".github/workflows/old.yml", "push", "success", "2026-11-20T08:00:00Z", "2026-11-20T08:10:00Z")}, ""},
	} {
		got, ok := resolve.PickRun(c.runs, wb)
		base := ""
		if ok {
			base = got.Path[strings.LastIndex(got.Path, "/")+1:]
		}
		if base != c.want {
			t.Errorf("%s: picked %q, want %q", name, base, c.want)
		}
	}
}

func TestRepoUsesThePackageAndCaches(t *testing.T) {
	var calls atomic.Int32
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/orgs/acme/packages/container/concierge": func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(`{"repository":{"full_name":"acme/concierge-ai"}}`))
		},
	})
	r := resolve.New(gh, slog.New(slog.DiscardHandler))

	a := argocd.App{Owner: "acme", Repo: "concierge", Image: "acme/concierge"}
	r.Repo(&a)
	r.Repo(&a)
	if a.Owner != "acme" || a.Repo != "concierge-ai" || calls.Load() != 1 {
		t.Fatalf("repo = %s/%s after %d calls, want acme/concierge-ai after 1", a.Owner, a.Repo, calls.Load())
	}

	explicit := argocd.App{Owner: "x", Repo: "y", Image: "acme/concierge", RepoExplicit: true}
	r.Repo(&explicit)
	if explicit.Repo != "y" || calls.Load() != 1 {
		t.Fatalf("an annotated repo was resolved: %+v (%d calls)", explicit, calls.Load())
	}
}

func TestRepoKeepsTheDerivedNameWhenTheLookupFails(t *testing.T) {
	var calls atomic.Int32
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/orgs/acme/packages/container/app": func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		},
	})
	var logs bytes.Buffer
	r := resolve.New(gh, slog.New(slog.NewTextHandler(&logs, nil)))
	for i := 0; i < 3; i++ {
		a := argocd.App{Owner: "acme", Repo: "app", Image: "acme/app"}
		r.Repo(&a)
		if a.Owner != "acme" || a.Repo != "app" {
			t.Fatalf("derived repo changed: %s/%s", a.Owner, a.Repo)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("failed lookup repeated: %d calls", calls.Load())
	}
	if n := strings.Count(logs.String(), "package repository lookup failed"); n != 1 {
		t.Fatalf("warned %d times, want once", n)
	}
}

func TestDiscoverFindsAndCachesTheWorkflow(t *testing.T) {
	var runCalls atomic.Int32
	m := metrics.New("test")
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{
		"/repos/o/r/commits/abc1234": ghtest.JSON(`{"sha":"abc1234full"}`),
		"/repos/o/r/actions/runs": func(w http.ResponseWriter, r *http.Request) {
			runCalls.Add(1)
			if r.URL.Query().Get("head_sha") != "abc1234full" {
				http.Error(w, "unexpected head_sha", http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`{"workflow_runs":[
				{"path":".github/workflows/e2e.yaml","event":"push","conclusion":"success","run_started_at":"2026-11-20T09:00:00Z","updated_at":"2026-11-20T09:02:00Z"},
				{"path":".github/workflows/ci-dev.yaml","event":"push","conclusion":"success","run_started_at":"2026-11-20T09:30:00Z","updated_at":"2026-11-20T09:58:58Z"}]}`))
		},
		"/repos/o/gitops/commits": ghtest.JSON(`[{"sha":"1","commit":{"message":"updates image o/r tag 'x' to 'dev-abc1234'","committer":{"date":"2026-11-20T09:59:00Z"}}}]`),
	})
	r := resolve.New(gh, slog.New(slog.DiscardHandler))
	a := argocd.App{Owner: "o", Repo: "r", Ref: "abc1234", Tag: "dev-abc1234",
		GitOpsOwner: "o", GitOpsRepo: "gitops", WriteBackFile: "ns/app/values-dev.yaml", WriteBackBranch: "dev"}
	for i := 0; i < 2; i++ {
		if got := r.Discover(context.Background(), a); got != "ci-dev.yaml" {
			t.Fatalf("Discover = %q, want ci-dev.yaml", got)
		}
	}
	if runCalls.Load() != 1 {
		t.Fatalf("discovery not cached: %d run lookups", runCalls.Load())
	}

	// Nothing to go on (no write-back file): empty, and it does not try again right away.
	none := argocd.App{Owner: "o", Repo: "unknown", Ref: "abc1234", Tag: "dev-abc1234"}
	if got := r.Discover(context.Background(), none); got != "" {
		t.Fatalf("Discover without evidence = %q", got)
	}
}
