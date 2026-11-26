package prs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tracked"
)

const (
	pageSize = 50
	maxSeen  = 100000
)

const query = `
query($owner: String!, $name: String!, $base: String!, $n: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequests(states: MERGED, baseRefName: $base, first: $n, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number createdAt mergedAt
        author { login }
        firstCommit: commits(first: 1) { nodes { commit { committedDate } } }
        recent: commits(last: 100) { nodes { commit { committedDate } } }
        head: commits(last: 1) { nodes { commit {
          committedDate
          statusCheckRollup { contexts(first: 100) { nodes {
            __typename
            ... on CheckRun { name startedAt completedAt conclusion }
            ... on StatusContext { context state }
          } } }
        } } }
        timeline: timelineItems(itemTypes: [READY_FOR_REVIEW_EVENT, CONVERT_TO_DRAFT_EVENT], last: 20) {
          nodes { __typename ... on ReadyForReviewEvent { createdAt } ... on ConvertToDraftEvent { createdAt } }
        }
        reviews(first: 10) { nodes { createdAt author { login } } }
      }
    }
  }
}`

// Poller observes PRs merged after it started. PRs merged earlier were observed by a previous
// process, or before the exporter existed, so they are not backfilled.
// ponytail: one page of the 50 most recently updated PRs per repo per poll; paginate if a
// repo ever merges more than that within a poll interval.
type Poller struct {
	gh          *ghclient.Client
	m           *metrics.Metrics
	log         *slog.Logger
	started     time.Time
	include     *regexp.Regexp
	firstReview bool
	seen        map[string]struct{}
	warned      map[string]struct{}
}

// New returns a Poller. checksInclude limits the `check` label; firstReview enables
// dora_pr_first_review_seconds.
func New(gh *ghclient.Client, m *metrics.Metrics, log *slog.Logger, started time.Time,
	checksInclude *regexp.Regexp, firstReview bool) *Poller {
	return &Poller{gh: gh, m: m, log: log, started: started, include: checksInclude,
		firstReview: firstReview, seen: map[string]struct{}{}, warned: map[string]struct{}{}}
}

// Poll reads recently merged PRs of every repo. Repo failures do not stop the others.
func (p *Poller) Poll(ctx context.Context, repos []tracked.Repo) error {
	var errs []error
	for _, r := range repos {
		var resp struct {
			Repository struct {
				PullRequests struct {
					Nodes []pullRequest `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		}
		vars := map[string]any{"owner": r.Owner, "name": r.Name, "base": r.DefaultBranch, "n": pageSize}
		if err := p.gh.GraphQL(ctx, query, vars, &resp); err != nil {
			if ghclient.IsNotFound(err) {
				// A repository that does not exist or is not visible to the App is a configuration
				// gap (wrong image to repo mapping), not an outage: say so once and move on.
				if _, done := p.warned[r.Owner+"/"+r.Name]; !done {
					p.warned[r.Owner+"/"+r.Name] = struct{}{}
					p.log.Warn("repository not found, skipping PR metrics", "repo", r.Owner+"/"+r.Name)
				}
				continue
			}
			errs = append(errs, fmt.Errorf("%s/%s: %w", r.Owner, r.Name, err))
			continue
		}
		for _, pr := range resp.Repository.PullRequests.Nodes {
			p.observe(r, pr)
		}
	}
	return errors.Join(errs...)
}

func (p *Poller) observe(r tracked.Repo, pr pullRequest) {
	key := fmt.Sprintf("%s/%s#%d", r.Owner, r.Name, pr.Number)
	if _, dup := p.seen[key]; dup || pr.MergedAt.IsZero() || pr.MergedAt.Before(p.started) {
		return
	}
	if len(p.seen) > maxSeen {
		p.seen = map[string]struct{}{} // ponytail: crude reset; PRs older than start are ignored anyway
	}
	p.seen[key] = struct{}{}

	res := compute(pr)
	repo, base := r.Name, r.DefaultBranch
	p.m.PRStage.WithLabelValues(repo, base, "coding").Observe(res.Coding.Seconds())
	p.m.PRStage.WithLabelValues(repo, base, "draft").Observe(res.Draft.Seconds())
	p.m.PRStage.WithLabelValues(repo, base, "review").Observe(res.Review.Seconds())
	p.m.PRHeadCommits.WithLabelValues(repo, base).Observe(float64(res.HeadCommits))
	if p.firstReview && res.HasFirstReview {
		p.m.PRFirstReview.WithLabelValues(repo, base).Observe(res.FirstReview.Seconds())
	}
	if res.HasChecks {
		p.m.PRChecks.WithLabelValues(repo, base).Observe(res.Checks.Seconds())
		p.m.PRCheckQueue.WithLabelValues(repo, base).Observe(res.Queue.Seconds())
		p.m.PRMergeDelay.WithLabelValues(repo, base).Observe(res.MergeDelay.Seconds())
	}
	for _, c := range res.Runs {
		name := c.Name
		if !p.include.MatchString(name) {
			name = "other"
		}
		p.m.PRCheckRuns.WithLabelValues(repo, base, name, c.Conclusion).Inc()
		if c.Timed {
			p.m.PRCheckDuration.WithLabelValues(repo, base, name, c.Conclusion).Observe(c.Duration.Seconds())
		}
	}
}
