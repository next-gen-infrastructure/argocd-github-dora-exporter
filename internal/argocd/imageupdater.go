package argocd

import (
	"path"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tag"
)

// Source is an image an ImageUpdater CR manages for an Application.
type Source struct {
	Image string // image name as configured, e.g. ghcr.io/owner/name
	Alias string // ImageUpdater alias of that image

	// Where image updater writes the new tag, from spec.writeBackConfig (git method only).
	WriteBackTarget string // e.g. "helmvalues:./values-dev.yaml"
	WriteBackBranch string // branch of the GitOps repository
}

type pattern struct {
	name   string // Application name pattern (glob)
	images []Source
}

// Store holds what the Argo CD Image Updater CRs say: which Applications get which image and
// where the new tag is written back. The CRD is v1alpha1; everything here is optional, so
// annotations remain a fallback.
type Store struct {
	mu  sync.RWMutex
	crs map[string][]pattern
}

// NewStore returns an empty Store.
func NewStore() *Store { return &Store{crs: map[string][]pattern{}} }

// Set records (or replaces) an ImageUpdater CR.
func (s *Store) Set(u *unstructured.Unstructured) {
	var target, branch string
	if m, _, _ := unstructured.NestedString(u.Object, "spec", "writeBackConfig", "method"); m == "git" || m == "" {
		target, _, _ = unstructured.NestedString(u.Object, "spec", "writeBackConfig", "gitConfig", "writeBackTarget")
		branch, _, _ = unstructured.NestedString(u.Object, "spec", "writeBackConfig", "gitConfig", "branch")
	}
	var ps []pattern
	refs, _, _ := unstructured.NestedSlice(u.Object, "spec", "applicationRefs")
	for _, r := range refs {
		m, _ := r.(map[string]any)
		p := pattern{name: str(m["namePattern"])}
		imgs, _ := m["images"].([]any)
		for _, i := range imgs {
			im, _ := i.(map[string]any)
			p.images = append(p.images, Source{
				Image: str(im["imageName"]), Alias: str(im["alias"]),
				WriteBackTarget: target, WriteBackBranch: branch,
			})
		}
		ps = append(ps, p)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.crs[u.GetName()] = ps
}

// Delete forgets an ImageUpdater CR.
func (s *Store) Delete(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.crs, name)
}

// Match returns the image an ImageUpdater CR manages for the Application. If the CR lists several
// images for it, wantImage (an image name, any registry) selects one; otherwise the first wins.
// A nil Store matches nothing.
func (s *Store) Match(app, wantImage string) (Source, bool) {
	if s == nil {
		return Source{}, false
	}
	wantPath, _ := tag.Split(wantImage)
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ps := range s.crs {
		for _, p := range ps {
			if ok, _ := path.Match(p.name, app); !ok {
				continue
			}
			for _, src := range p.images {
				if got, _ := tag.Split(src.Image); wantImage == "" || got == wantPath {
					return src, true
				}
			}
		}
	}
	return Source{}, false
}

// WriteBackFile returns the repository path of the file image updater writes the tag to, for an
// Application whose source path is sourcePath. Only the helmvalues target is understood; any
// other target returns "".
func (src Source) WriteBackFile(sourcePath string) string {
	file, ok := strings.CutPrefix(src.WriteBackTarget, "helmvalues:")
	if !ok || file == "" {
		return ""
	}
	if strings.HasPrefix(file, "/") { // relative to the repository root
		return strings.TrimPrefix(path.Clean(file), "/")
	}
	return path.Join(sourcePath, file)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
