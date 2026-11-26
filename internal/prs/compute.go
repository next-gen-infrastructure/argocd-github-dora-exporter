// Package prs polls merged pull requests and derives PR stages and check timings.
package prs

import (
	"strings"
	"time"
)

type timeNode struct {
	CommittedDate time.Time `json:"committedDate"`
}

type commitNodes struct {
	Nodes []struct {
		Commit timeNode `json:"commit"`
	} `json:"nodes"`
}

type checkContext struct {
	Typename    string     `json:"__typename"`
	Name        string     `json:"name"`
	StartedAt   *time.Time `json:"startedAt"`
	CompletedAt *time.Time `json:"completedAt"`
	Conclusion  string     `json:"conclusion"`
	Context     string     `json:"context"`
	State       string     `json:"state"`
}

type pullRequest struct {
	Number    int       `json:"number"`
	CreatedAt time.Time `json:"createdAt"`
	MergedAt  time.Time `json:"mergedAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
	FirstCommit commitNodes `json:"firstCommit"`
	Recent      commitNodes `json:"recent"`
	Head        struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     time.Time `json:"committedDate"`
				StatusCheckRollup *struct {
					Contexts struct {
						Nodes []checkContext `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"head"`
	Timeline struct {
		Nodes []struct {
			Typename  string    `json:"__typename"`
			CreatedAt time.Time `json:"createdAt"`
		} `json:"nodes"`
	} `json:"timeline"`
	Reviews struct {
		Nodes []struct {
			CreatedAt time.Time `json:"createdAt"`
			Author    *struct {
				Login string `json:"login"`
			} `json:"author"`
		} `json:"nodes"`
	} `json:"reviews"`
}

// checkRun is one entry of the final head commit's checks.
type checkRun struct {
	Name       string
	Conclusion string // lower case
	Duration   time.Duration
	Timed      bool
}

// result holds everything derived from one merged PR.
type result struct {
	Coding, Draft, Review time.Duration

	HasChecks  bool
	Checks     time.Duration // earliest start to latest completion
	Queue      time.Duration // head commit date to earliest start
	MergeDelay time.Duration // latest completion to merge
	Runs       []checkRun

	HeadCommits    int
	HasFirstReview bool
	FirstReview    time.Duration
}

const readyEvent = "ReadyForReviewEvent"

// compute derives stages and check timings. The review starts at the last ready-for-review
// event before the merge, or at creation if the PR was never a draft.
func compute(pr pullRequest) result {
	var r result

	reviewStart := pr.CreatedAt
	for _, e := range pr.Timeline.Nodes {
		if e.Typename == readyEvent && e.CreatedAt.After(reviewStart) && !e.CreatedAt.After(pr.MergedAt) {
			reviewStart = e.CreatedAt
		}
	}
	if len(pr.FirstCommit.Nodes) > 0 {
		r.Coding = nonNegative(pr.CreatedAt.Sub(pr.FirstCommit.Nodes[0].Commit.CommittedDate))
	}
	r.Draft = nonNegative(reviewStart.Sub(pr.CreatedAt))
	r.Review = nonNegative(pr.MergedAt.Sub(reviewStart))

	r.HeadCommits = 1
	for i, n := range pr.Recent.Nodes {
		if i < len(pr.Recent.Nodes)-1 && n.Commit.CommittedDate.After(reviewStart) {
			r.HeadCommits++
		}
	}

	if pr.Author != nil {
		for _, rv := range pr.Reviews.Nodes {
			if rv.Author != nil && rv.Author.Login != pr.Author.Login {
				r.HasFirstReview = true
				r.FirstReview = nonNegative(rv.CreatedAt.Sub(reviewStart))
				break
			}
		}
	}

	if len(pr.Head.Nodes) == 0 || pr.Head.Nodes[0].Commit.StatusCheckRollup == nil {
		return r
	}
	head := pr.Head.Nodes[0].Commit
	var first, last time.Time
	for _, c := range head.StatusCheckRollup.Contexts.Nodes {
		switch c.Typename {
		case "CheckRun":
			run := checkRun{Name: c.Name, Conclusion: strings.ToLower(c.Conclusion)}
			if c.StartedAt != nil && c.CompletedAt != nil && !c.StartedAt.IsZero() && !c.CompletedAt.IsZero() {
				run.Timed = true
				run.Duration = nonNegative(c.CompletedAt.Sub(*c.StartedAt))
				if first.IsZero() || c.StartedAt.Before(first) {
					first = *c.StartedAt
				}
				if c.CompletedAt.After(last) {
					last = *c.CompletedAt
				}
			}
			if run.Conclusion == "" {
				run.Conclusion = "pending"
			}
			r.Runs = append(r.Runs, run)
		case "StatusContext":
			// Legacy commit status: counted, but it has no start time.
			r.Runs = append(r.Runs, checkRun{Name: c.Context, Conclusion: strings.ToLower(c.State)})
		}
	}
	if !first.IsZero() {
		r.HasChecks = true
		r.Checks = nonNegative(last.Sub(first))
		r.Queue = nonNegative(first.Sub(head.CommittedDate))
		r.MergeDelay = nonNegative(pr.MergedAt.Sub(last))
	}
	return r
}

func nonNegative(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}
