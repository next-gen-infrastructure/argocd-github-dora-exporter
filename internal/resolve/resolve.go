// Package resolve finds facts about an application that are not in its Argo CD resource: the real
// GitHub repository of its image, and the workflow that builds the image.
package resolve

import (
	"context"
	"log/slog"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
)

const (
	hitTTL  = 6 * time.Hour
	missTTL = time.Hour
	// windowAfter is how long after a run finished image updater may still write the tag back
	// (it polls the registry), so the write-back of that run's image falls inside it.
	windowAfter = 5 * time.Minute
)

type entry struct {
	val   string
	until time.Time
}

// Resolver caches what it finds. A failed lookup is cached for a shorter time, so a missing
// permission does not cost a request per event.
type Resolver struct {
	gh     *ghclient.Client
	log    *slog.Logger
	now    func() time.Time
	mu     sync.Mutex
	repos  map[string]entry // image path -> "owner/name", "" when unknown
	flows  map[string]entry // "owner/name" -> workflow file, "" when unknown
	warned map[string]struct{}
}

// New returns a Resolver.
func New(gh *ghclient.Client, log *slog.Logger) *Resolver {
	return &Resolver{gh: gh, log: log, now: time.Now, repos: map[string]entry{},
		flows: map[string]entry{}, warned: map[string]struct{}{}}
}

// Repo replaces the repository derived from the image path with the one the GHCR package is linked
// to. The repo annotation wins, and a failed lookup keeps the derived repository.
// It may call GitHub (once per image per TTL), so it blocks the caller on a cache miss.
func (r *Resolver) Repo(a *argocd.App) {
	if a.RepoExplicit || a.Image == "" {
		return
	}
	if full := r.lookup(r.repos, a.Image, func(ctx context.Context) (string, error) {
		owner, pkg, _ := strings.Cut(a.Image, "/")
		return r.gh.PackageRepo(ctx, owner, pkg)
	}, "package repository lookup failed, keeping the repository derived from the image"); full != "" {
		if owner, name, ok := strings.Cut(full, "/"); ok {
			a.Owner, a.Repo = owner, name
		}
	}
}

// Workflow returns the workflow learned or discovered for the repository, or "".
func (r *Resolver) Workflow(repo string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.flows[repo]; ok && r.now().Before(e.until) {
		return e.val
	}
	return ""
}

// Learn records the package workflow of a repository.
func (r *Resolver) Learn(repo, workflow string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flows[repo] = entry{workflow, r.now().Add(hitTTL)}
}

// Discover finds the package workflow of the repository from the application's current image:
// the run of the deployed commit that was running when image updater wrote the tag back.
func (r *Resolver) Discover(ctx context.Context, a argocd.App) string {
	key := a.Owner + "/" + a.Repo
	if wf := r.Workflow(key); wf != "" {
		return wf
	}
	r.mu.Lock()
	e, ok := r.flows[key]
	r.mu.Unlock()
	if ok && r.now().Before(e.until) { // a recent failed discovery
		return ""
	}
	wf := r.discover(ctx, a)
	r.mu.Lock()
	defer r.mu.Unlock()
	if wf == "" {
		r.flows[key] = entry{"", r.now().Add(missTTL)}
	} else {
		r.flows[key] = entry{wf, r.now().Add(hitTTL)}
	}
	return wf
}

func (r *Resolver) discover(ctx context.Context, a argocd.App) string {
	if a.Ref == "" || a.Tag == "" {
		return ""
	}
	wb, err := WriteBackAt(ctx, r.gh, a)
	if err != nil || wb.IsZero() {
		return ""
	}
	sha, err := r.gh.CommitSHA(ctx, a.Owner, a.Repo, a.Ref)
	if err != nil {
		return ""
	}
	runs, err := r.gh.RunsForSHA(ctx, a.Owner, a.Repo, sha)
	if err != nil {
		return ""
	}
	if run, ok := PickRun(runs, wb); ok {
		return path.Base(run.Path)
	}
	return ""
}

// lookup caches a string lookup; failures and empty answers are cached for missTTL and warned
// about once per key.
func (r *Resolver) lookup(cache map[string]entry, key string, fetch func(context.Context) (string, error), warn string) string {
	r.mu.Lock()
	if e, ok := cache[key]; ok && r.now().Before(e.until) {
		r.mu.Unlock()
		return e.val
	}
	r.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	val, err := fetch(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil || val == "" {
		cache[key] = entry{"", r.now().Add(missTTL)}
		if _, done := r.warned[key]; !done {
			r.warned[key] = struct{}{}
			r.log.Warn(warn, "key", key, "error", err)
		}
		return ""
	}
	cache[key] = entry{val, r.now().Add(hitTTL)}
	return val
}

// WriteBackAt returns when image updater wrote a.Tag to the GitOps repository: the newest commit
// that touched the values file and set the tag ("... tag 'old' to 'new'"). A later commit that
// replaced it mentions the tag as the old one and must not match. It returns the zero time when
// the file is unknown or no such commit exists.
func WriteBackAt(ctx context.Context, gh *ghclient.Client, a argocd.App) (time.Time, error) {
	if a.WriteBackFile == "" || a.GitOpsRepo == "" || a.Tag == "" {
		return time.Time{}, nil
	}
	commits, err := gh.PathCommits(ctx, a.GitOpsOwner, a.GitOpsRepo, a.WriteBackBranch, a.WriteBackFile, 10)
	if err != nil {
		return time.Time{}, err
	}
	for _, c := range commits { // newest first
		if strings.Contains(c.Commit.Message, "to '"+a.Tag+"'") {
			return c.Commit.Committer.Date, nil
		}
	}
	return time.Time{}, nil
}

// PickRun returns the run that built the image written back at writeBack: not triggered by a pull
// request, successful, started before the write-back and finished no more than windowAfter
// before it. If several qualify, the one that finished closest to the write-back wins.
func PickRun(runs []ghclient.Run, writeBack time.Time) (ghclient.Run, bool) {
	var best ghclient.Run
	found := false
	var bestGap time.Duration
	for _, run := range runs {
		if run.IsPullRequestRun() || run.Conclusion != "success" || run.Path == "" {
			continue
		}
		if run.RunStartedAt.After(writeBack) || run.UpdatedAt.Add(windowAfter).Before(writeBack) {
			continue
		}
		gap := writeBack.Sub(run.UpdatedAt)
		if gap < 0 {
			gap = -gap
		}
		if !found || gap < bestGap {
			best, bestGap, found = run, gap, true
		}
	}
	return best, found
}
