// Package leadtime computes per-commit lead time and the rollout stage for new deployments.
package leadtime

import (
	"context"
	"log/slog"
	"net/url"
	"path"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/deploy"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/resolve"
)

const (
	maxCommits = 1000
	maxSeen    = 100000
)

// Worker processes deployments off the informer goroutine, since it calls GitHub.
type Worker struct {
	gh   *ghclient.Client
	res  *resolve.Resolver
	m    *metrics.Metrics
	log  *slog.Logger
	ch   chan deploy.Deployed
	seen map[string]struct{} // owner/repo@sha already observed; only used by Run
}

// New returns a Worker. res learns the package workflow of repositories as a side effect.
func New(gh *ghclient.Client, res *resolve.Resolver, m *metrics.Metrics, log *slog.Logger) *Worker {
	return &Worker{gh: gh, res: res, m: m, log: log, ch: make(chan deploy.Deployed, 64), seen: map[string]struct{}{}}
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

// Handle observes one lead time per not-yet-seen non-merge commit in PrevRef...Ref, then the
// rollout and image update timings when the package build can be found.
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

	if a.PackageWorkflow == "" && a.WorkflowDefault == "" && a.WriteBackFile == "" {
		return nil // nothing to find the package build with
	}
	head := ""
	if len(commits) > 0 {
		head = commits[len(commits)-1].SHA
	} else if head, err = w.gh.CommitSHA(ctx, a.Owner, a.Repo, a.Ref); err != nil {
		return err
	}
	return w.packageStages(ctx, d, head)
}

// packageStages observes the rollout (package build finished to sync) and, when the write-back
// commit is known, the image update (package build finished to write-back).
func (w *Worker) packageStages(ctx context.Context, d deploy.Deployed, head string) error {
	a := d.App
	writeBack, err := resolve.WriteBackAt(ctx, w.gh, a)
	if err != nil { // only the image update stage and the discovery depend on it
		w.log.Warn("image update lookup failed", "app", a.Name, "error", err)
		writeBack = time.Time{}
	}
	done, err := w.packageDone(ctx, a, head, writeBack)
	if err != nil || done.IsZero() {
		return err
	}
	w.m.Rollout.WithLabelValues(a.Repo, a.Name).Observe(nonNegative(d.At.Sub(done)))
	if !writeBack.IsZero() {
		w.m.ImageUpdate.WithLabelValues(a.Repo, a.Name).Observe(nonNegative(writeBack.Sub(done)))
	}
	return nil
}

// packageDone returns when the package build of the commit finished, or the zero time. With a
// package-workflow annotation that workflow is used; otherwise the run is found from the
// write-back time, and the PACKAGE_WORKFLOW default is the last resort.
func (w *Worker) packageDone(ctx context.Context, a argocd.App, head string, writeBack time.Time) (time.Time, error) {
	if a.PackageWorkflow != "" {
		return w.doneByWorkflow(ctx, a, a.PackageWorkflow, head)
	}
	if !writeBack.IsZero() {
		runs, err := w.gh.RunsForSHA(ctx, a.Owner, a.Repo, head)
		if err != nil && !ghclient.IsNotFound(err) {
			return time.Time{}, err
		}
		if run, ok := resolve.PickRun(runs, writeBack); ok {
			w.res.Learn(a.Owner+"/"+a.Repo, path.Base(run.Path))
			return run.UpdatedAt, nil
		}
	}
	if a.WorkflowDefault != "" {
		return w.doneByWorkflow(ctx, a, a.WorkflowDefault, head)
	}
	return time.Time{}, nil
}

// doneByWorkflow returns when the first successful run of the workflow for the commit finished.
// The image was pushed by that run; later runs (re-dispatches, re-runs) finish after the
// deployment and must not count.
func (w *Worker) doneByWorkflow(ctx context.Context, a argocd.App, workflow, head string) (time.Time, error) {
	runs, err := w.gh.WorkflowRuns(ctx, a.Owner, a.Repo, workflow,
		url.Values{"head_sha": {head}, "status": {"success"}, "per_page": {"10"}})
	if ghclient.IsNotFound(err) {
		return time.Time{}, nil // no such workflow: there is just no rollout stage
	}
	if err != nil {
		return time.Time{}, err
	}
	var done time.Time
	for _, r := range runs {
		if r.IsPullRequestRun() {
			continue
		}
		if done.IsZero() || r.UpdatedAt.Before(done) {
			done = r.UpdatedAt
		}
	}
	return done, nil
}

func nonNegative(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}
