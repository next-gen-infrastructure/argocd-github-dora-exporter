// Package builds observes runner queue time and build duration of the package workflow.
package builds

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tracked"
)

const maxSeen = 100000

// Poller observes jobs of completed package workflow runs created after it started.
// ponytail: looks at the 50 newest completed runs per repo per poll; runs that were still in
// progress when a poll happened are picked up by the next one.
type Poller struct {
	gh      *ghclient.Client
	m       *metrics.Metrics
	started time.Time
	seen    map[int64]struct{}
}

// New returns a Poller.
func New(gh *ghclient.Client, m *metrics.Metrics, started time.Time) *Poller {
	return &Poller{gh: gh, m: m, started: started, seen: map[int64]struct{}{}}
}

// Poll reads new completed runs of each repo's package workflow. Repo failures do not stop
// the others.
func (p *Poller) Poll(ctx context.Context, repos []tracked.Repo) error {
	var errs []error
	for _, r := range repos {
		if r.PackageWorkflow == "" {
			continue
		}
		if err := p.pollRepo(ctx, r); err != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.Owner, r.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (p *Poller) pollRepo(ctx context.Context, r tracked.Repo) error {
	runs, err := p.gh.WorkflowRuns(ctx, r.Owner, r.Name, r.PackageWorkflow, url.Values{
		"status":   {"completed"},
		"created":  {">=" + p.started.UTC().Format("2006-01-02T15:04:05Z")},
		"per_page": {"50"},
	})
	if err != nil {
		return err
	}
	if len(p.seen) > maxSeen {
		p.seen = map[int64]struct{}{} // ponytail: crude reset; runs before start are filtered anyway
	}
	for _, run := range runs {
		if _, dup := p.seen[run.ID]; dup {
			continue
		}
		jobs, err := p.gh.Jobs(ctx, r.Owner, r.Name, run.ID)
		if err != nil {
			return err
		}
		p.seen[run.ID] = struct{}{}
		for _, j := range jobs {
			c := strings.ToLower(j.Conclusion)
			if c == "" || c == "skipped" || j.StartedAt.IsZero() || j.CompletedAt.IsZero() {
				continue
			}
			p.m.BuildQueue.WithLabelValues(r.Name, r.PackageWorkflow, c).Observe(nonNegative(j.StartedAt.Sub(j.CreatedAt)))
			p.m.BuildDuration.WithLabelValues(r.Name, r.PackageWorkflow, c).Observe(nonNegative(j.CompletedAt.Sub(j.StartedAt)))
		}
	}
	return nil
}

func nonNegative(d time.Duration) float64 {
	if d < 0 {
		return 0
	}
	return d.Seconds()
}
