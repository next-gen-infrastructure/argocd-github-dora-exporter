package ghclient

import (
	"context"
	"fmt"
	"net/url"
	"time"
)

// Commit is a commit as returned by the compare API.
type Commit struct {
	SHA     string `json:"sha"`
	Parents []struct {
		SHA string `json:"sha"`
	} `json:"parents"`
	Commit struct {
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// Compare returns the commits in base...head, oldest first, at most max of them.
func (c *Client) Compare(ctx context.Context, owner, repo, base, head string, max int) ([]Commit, error) {
	var all []Commit
	for page := 1; len(all) < max; page++ {
		var r struct {
			Commits []Commit `json:"commits"`
		}
		path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s?per_page=100&page=%d",
			owner, repo, url.PathEscape(base), url.PathEscape(head), page)
		if err := c.REST(ctx, path, &r); err != nil {
			return nil, err
		}
		all = append(all, r.Commits...)
		if len(r.Commits) < 100 {
			break
		}
	}
	if len(all) > max {
		all = all[:max]
	}
	return all, nil
}

// CommitSHA resolves a (possibly short) ref to a full commit sha.
func (c *Client) CommitSHA(ctx context.Context, owner, repo, ref string) (string, error) {
	var r struct {
		SHA string `json:"sha"`
	}
	err := c.REST(ctx, fmt.Sprintf("/repos/%s/%s/commits/%s", owner, repo, url.PathEscape(ref)), &r)
	return r.SHA, err
}

// Run is a workflow run.
type Run struct {
	ID           int64     `json:"id"`
	Path         string    `json:"path"` // .github/workflows/<file>
	Event        string    `json:"event"`
	Conclusion   string    `json:"conclusion"`
	HeadSHA      string    `json:"head_sha"`
	CreatedAt    time.Time `json:"created_at"`
	RunStartedAt time.Time `json:"run_started_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// WorkflowRuns lists runs of a workflow file; q holds the API filters (status, head_sha, ...).
func (c *Client) WorkflowRuns(ctx context.Context, owner, repo, workflow string, q url.Values) ([]Run, error) {
	var r struct {
		Runs []Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/runs?%s", owner, repo, url.PathEscape(workflow), q.Encode())
	err := c.REST(ctx, path, &r)
	return r.Runs, err
}

// Job is a workflow run job.
type Job struct {
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	CreatedAt   time.Time `json:"created_at"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// Jobs lists the jobs of a run.
func (c *Client) Jobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	var r struct {
		Jobs []Job `json:"jobs"`
	}
	err := c.REST(ctx, fmt.Sprintf("/repos/%s/%s/actions/runs/%d/jobs?per_page=100", owner, repo, runID), &r)
	return r.Jobs, err
}

// PathCommit is a commit from the commit list.
type PathCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message   string `json:"message"`
		Committer struct {
			Date time.Time `json:"date"`
		} `json:"committer"`
	} `json:"commit"`
}

// PathCommits returns the newest n commits on branch that touched path.
func (c *Client) PathCommits(ctx context.Context, owner, repo, branch, path string, n int) ([]PathCommit, error) {
	q := url.Values{"sha": {branch}, "path": {path}, "per_page": {fmt.Sprint(n)}}
	var out []PathCommit
	err := c.REST(ctx, fmt.Sprintf("/repos/%s/%s/commits?%s", owner, repo, q.Encode()), &out)
	return out, err
}

// IsPullRequestRun reports whether the run was triggered by a pull request. Those runs validate a
// change; they are not package builds, even when the same workflow also builds on push.
func (r Run) IsPullRequestRun() bool {
	return r.Event == "pull_request" || r.Event == "pull_request_target"
}

// RunsForSHA lists the workflow runs of every workflow for a full commit sha.
func (c *Client) RunsForSHA(ctx context.Context, owner, repo, sha string) ([]Run, error) {
	var r struct {
		Runs []Run `json:"workflow_runs"`
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/runs?head_sha=%s&per_page=100", owner, repo, url.QueryEscape(sha))
	err := c.REST(ctx, path, &r)
	return r.Runs, err
}

// PackageRepo returns the repository ("owner/name") a GHCR container package is linked to, or ""
// if it is not linked. It needs the Packages read permission.
func (c *Client) PackageRepo(ctx context.Context, owner, name string) (string, error) {
	var r struct {
		Repository *struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	esc := url.PathEscape(name) // a package name may contain "/"
	err := c.REST(ctx, fmt.Sprintf("/orgs/%s/packages/container/%s", owner, esc), &r)
	if IsNotFound(err) { // the owner may be a user, not an organisation
		err = c.REST(ctx, fmt.Sprintf("/users/%s/packages/container/%s", owner, esc), &r)
	}
	if err != nil || r.Repository == nil {
		return "", err
	}
	return r.Repository.FullName, nil
}
