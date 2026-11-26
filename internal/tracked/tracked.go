// Package tracked is the set of GitHub repositories discovered from opted-in Applications.
package tracked

import (
	"sort"
	"sync"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
)

// Repo is a tracked repository and its per-environment settings.
type Repo struct {
	Owner, Name     string
	DefaultBranch   string
	PackageWorkflow string // effective workflow, set by the poll loop; empty means none known

	// App is the latest observed Application of the repository, used to discover the workflow.
	App argocd.App
}

// Set is a concurrent set of repositories keyed by owner/name.
type Set struct {
	mu sync.Mutex
	m  map[string]Repo
}

// New returns an empty Set.
func New() *Set { return &Set{m: map[string]Repo{}} }

// Add inserts or replaces r.
func (s *Set) Add(r Repo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[r.Owner+"/"+r.Name] = r
}

// List returns the repositories sorted by owner/name.
func (s *Set) List() []Repo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Repo, 0, len(s.m))
	for _, r := range s.m {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Owner+"/"+out[i].Name < out[j].Owner+"/"+out[j].Name
	})
	return out
}
