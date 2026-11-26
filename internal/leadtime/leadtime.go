// Package leadtime computes per-commit lead time and the rollout stage for new deployments.
package leadtime

import (
	"context"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/deploy"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

const (
	maxCommits = 1000
	maxSeen    = 100000
)

// Worker processes deployments off the informer goroutine, since it calls GitHub.
type Worker struct {
	gh   *ghclient.Client
	m    *metrics.Metrics
	log  *slog.Logger
	ch   chan deploy.Deployed
	seen map[string]struct{} // owner/repo@sha already observed; only used by Run
}

// New returns a Worker.
func New(gh *ghclient.Client, m *metrics.Metrics, log *slog.Logger) *Worker {
	return &Worker{gh: gh, m: m, log: log, ch: make(chan deploy.Deployed, 64), seen: map[string]struct{}{}}
}

// Submit queues a deployment without blocking; a full queue drops it.
func (w *Worker) Submit(d deploy.Deployed) {
	select {
	case w.ch <- d:
	default:
		w.log.Warn("lead time queue full, dropping deployment", "app", d.App.Name)
	}
}

// Run handles queued deployments until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case d := <-w.ch:
			if err := w.Handle(ctx, d); err != nil {
				w.log.Warn("lead time failed", "app", d.App.Name, "prev", d.PrevRef, "ref", d.App.Ref, "error", err)
			}
		}
	}
}

// Handle observes one lead time per not-yet-seen non-merge commit in PrevRef...Ref and, when a
// package workflow is configured, the rollout time.
func (w *Worker) Handle(ctx context.Context, d deploy.Deployed) error {
	a := d.App
	commits, err := w.gh.Compare(ctx, a.Owner, a.Repo, d.PrevRef, a.Ref, maxCommits)
	if err != nil {
		return err
	}
	if len(w.seen) > maxSeen {
		w.seen = map[string]struct{}{} // ponytail: crude reset; an LRU if re-observation shows up
	}
	for _, c := range commits {
		if len(c.Parents) > 1 {
			continue
		}
		key := a.Owner + "/" + a.Repo + "@" + c.SHA
		if _, dup := w.seen[key]; dup {
			continue
		}
		w.seen[key] = struct{}{}
		w.m.LeadTime.WithLabelValues(a.Repo).Observe(nonNegative(d.At.Sub(c.Commit.Committer.Date)))
	}

	if a.PackageWorkflow == "" {
		return nil
	}
	head := ""
	if len(commits) > 0 {
		head = commits[len(commits)-1].SHA
	} else if head, err = w.gh.CommitSHA(ctx, a.Owner, a.Repo, a.Ref); err != nil {
		return err
	}
	runs, err := w.gh.WorkflowRuns(ctx, a.Owner, a.Repo, a.PackageWorkflow,
		url.Values{"head_sha": {head}, "status": {"success"}, "per_page": {"10"}})
	if err != nil {
		return err
	}
	// The image was pushed by the first successful run for that commit; later runs (re-dispatches,
	// re-runs) finish after the deployment and must not count.
	var done time.Time
	for _, r := range runs {
		if done.IsZero() || r.UpdatedAt.Before(done) {
			done = r.UpdatedAt
		}
	}
	if !done.IsZero() {
		w.m.Rollout.WithLabelValues(a.Repo, a.Name).Observe(nonNegative(d.At.Sub(done)))
		w.observeImageUpdate(ctx, a, done)
	}
	return nil
}

func nonNegative(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}

// observeImageUpdate records when image updater wrote a.Tag to the GitOps repository, relative
// to the package run finishing at done. The write-back is the newest commit that touched the
// values file and set the tag ("... tag 'old' to 'new'"); a later commit that replaced it
// mentions it as the old tag and must not match. Failures only skip this one metric.
func (w *Worker) observeImageUpdate(ctx context.Context, a argocd.App, done time.Time) {
	if a.WriteBackFile == "" || a.GitOpsRepo == "" {
		return
	}
	commits, err := w.gh.PathCommits(ctx, a.GitOpsOwner, a.GitOpsRepo, a.WriteBackBranch, a.WriteBackFile, 10)
	if err != nil {
		w.log.Warn("image update lookup failed", "app", a.Name, "error", err)
		return
	}
	for _, c := range commits { // newest first
		if strings.Contains(c.Commit.Message, "to '"+a.Tag+"'") {
			w.m.ImageUpdate.WithLabelValues(a.Repo, a.Name).Observe(nonNegative(c.Commit.Committer.Date.Sub(done)))
			return
		}
	}
}
