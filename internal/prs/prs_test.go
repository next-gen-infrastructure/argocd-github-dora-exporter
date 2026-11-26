package prs

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghtest"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tracked"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// prJSON: first commit 08:00, PR opened 09:00 as draft, ready 11:00 (after a draft round trip),
// head commit 11:30, checks 11:31-11:41 (lint) and 11:32-11:36 (unit), merged 12:00.
const prJSON = `{
 "number": 7, "createdAt": "2026-11-20T09:00:00Z", "mergedAt": "2026-11-20T12:00:00Z",
 "author": {"login": "alice"},
 "firstCommit": {"nodes": [{"commit": {"committedDate": "2026-11-20T08:00:00Z"}}]},
 "recent": {"nodes": [
   {"commit": {"committedDate": "2026-11-20T08:00:00Z"}},
   {"commit": {"committedDate": "2026-11-20T10:00:00Z"}},
   {"commit": {"committedDate": "2026-11-20T11:15:00Z"}},
   {"commit": {"committedDate": "2026-11-20T11:30:00Z"}}]},
 "head": {"nodes": [{"commit": {"committedDate": "2026-11-20T11:30:00Z", "statusCheckRollup": {"contexts": {"nodes": [
   {"__typename": "CheckRun", "name": "lint", "startedAt": "2026-11-20T11:31:00Z", "completedAt": "2026-11-20T11:41:00Z", "conclusion": "SUCCESS"},
   {"__typename": "CheckRun", "name": "unit", "startedAt": "2026-11-20T11:32:00Z", "completedAt": "2026-11-20T11:36:00Z", "conclusion": "FAILURE"},
   {"__typename": "StatusContext", "context": "legacy/ci", "state": "SUCCESS"}]}}}}]},
 "timeline": {"nodes": [
   {"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-20T09:30:00Z"},
   {"__typename": "ConvertToDraftEvent", "createdAt": "2026-11-20T10:00:00Z"},
   {"__typename": "ReadyForReviewEvent", "createdAt": "2026-11-20T11:00:00Z"}]},
 "reviews": {"nodes": [
   {"createdAt": "2026-11-20T10:30:00Z", "author": {"login": "alice"}},
   {"createdAt": "2026-11-20T11:20:00Z", "author": {"login": "bob"}}]}
}`

func TestCompute(t *testing.T) {
	var pr pullRequest
	if err := json.Unmarshal([]byte(prJSON), &pr); err != nil {
		t.Fatal(err)
	}
	r := compute(pr)
	h := time.Hour
	if r.Coding != h || r.Draft != 2*h || r.Review != h {
		t.Errorf("stages = %v %v %v, want 1h 2h 1h", r.Coding, r.Draft, r.Review)
	}
	if !r.HasChecks || r.Checks != 10*time.Minute || r.Queue != time.Minute || r.MergeDelay != 19*time.Minute {
		t.Errorf("checks = %+v", r)
	}
	// commits after the 11:00 review start: 11:15 and the head commit; head counted once.
	if r.HeadCommits != 2 {
		t.Errorf("HeadCommits = %d, want 2", r.HeadCommits)
	}
	if !r.HasFirstReview || r.FirstReview != 20*time.Minute { // bob at 11:20, not alice
		t.Errorf("first review = %v %v", r.HasFirstReview, r.FirstReview)
	}
	if len(r.Runs) != 3 || r.Runs[0].Conclusion != "success" || !r.Runs[0].Timed ||
		r.Runs[1].Conclusion != "failure" || r.Runs[2].Name != "legacy/ci" || r.Runs[2].Timed {
		t.Errorf("runs = %+v", r.Runs)
	}
}

func TestComputeNeverDraft(t *testing.T) {
	pr := pullRequest{CreatedAt: ts("2026-11-20T09:00:00Z"), MergedAt: ts("2026-11-20T10:00:00Z")}
	r := compute(pr)
	if r.Draft != 0 || r.Review != time.Hour || r.HeadCommits != 1 || r.HasChecks {
		t.Errorf("result = %+v", r)
	}
}

func TestPollSkipsOldAndDuplicatePRs(t *testing.T) {
	m := metrics.New("test")
	resp := `{"data":{"repository":{"pullRequests":{"nodes":[` + prJSON + `]}}}}`
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{"/graphql": ghtest.JSON(resp)})
	repos := []tracked.Repo{{Owner: "o", Name: "r", DefaultBranch: "dev"}}
	re := regexp.MustCompile(`^(lint|unit)$`)

	// PR merged before start: ignored.
	p := New(gh, m, slog.New(slog.DiscardHandler), ts("2026-11-21T00:00:00Z"), re, true)
	if err := p.Poll(context.Background(), repos); err != nil {
		t.Fatal(err)
	}
	if n := ghtest.HistCount(t, m, "dora_pr_stage_seconds"); n != 0 {
		t.Fatalf("old PR observed %d times", n)
	}

	// Merged after start: observed once, even if polled twice.
	p = New(gh, m, slog.New(slog.DiscardHandler), ts("2026-11-20T00:00:00Z"), re, true)
	for i := 0; i < 2; i++ {
		if err := p.Poll(context.Background(), repos); err != nil {
			t.Fatal(err)
		}
	}
	if n := ghtest.HistCount(t, m, "dora_pr_stage_seconds"); n != 3 {
		t.Fatalf("stage observations = %d, want 3 (coding, draft, review)", n)
	}
	if n := ghtest.HistCount(t, m, "dora_pr_first_review_seconds"); n != 1 {
		t.Fatalf("first review observations = %d", n)
	}
	if n := ghtest.HistCount(t, m, "dora_pr_check_duration_seconds"); n != 2 { // legacy status is untimed
		t.Fatalf("check duration observations = %d, want 2", n)
	}
}

func TestPollMissingRepoDoesNotFailThePoll(t *testing.T) {
	m := metrics.New("test")
	ok := `{"data":{"repository":{"pullRequests":{"nodes":[` + prJSON + `]}}}}`
	gh := ghtest.New(t, m, map[string]http.HandlerFunc{"/graphql": func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"name":"missing"`) {
			_, _ = w.Write([]byte(`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","message":"Could not resolve to a Repository"}]}`))
			return
		}
		_, _ = w.Write([]byte(ok))
	}})
	var logs bytes.Buffer
	p := New(gh, m, slog.New(slog.NewTextHandler(&logs, nil)), ts("2026-11-20T00:00:00Z"), regexp.MustCompile(`.*`), false)
	repos := []tracked.Repo{{Owner: "o", Name: "missing", DefaultBranch: "dev"}, {Owner: "o", Name: "r", DefaultBranch: "dev"}}
	for i := 0; i < 2; i++ {
		if err := p.Poll(context.Background(), repos); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	if n := strings.Count(logs.String(), "repository not found"); n != 1 {
		t.Fatalf("warned %d times, want once", n)
	}
	if n := ghtest.HistCount(t, m, "dora_pr_stage_seconds"); n != 3 {
		t.Fatalf("the existing repo was not observed: %d", n)
	}
}
