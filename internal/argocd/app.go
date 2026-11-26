// Package argocd reads Argo CD Applications and turns them into App values.
package argocd

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/config"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tag"
)

// App is the part of an Application the exporter cares about.
type App struct {
	Name            string
	Alias           string // ImageUpdater alias of the tracked image, when known
	Owner, Repo     string // GitHub owner and repository name
	Image           string // tracked image path without registry host
	Tag, Ref        string // deployed image tag and the commit ref extracted from it
	DefaultBranch   string
	PackageWorkflow string

	// GitOps repository the Application deploys from, and where image updater writes the tag.
	GitOpsOwner, GitOpsRepo string
	WriteBackFile           string // repository path of the values file; empty if unknown
	WriteBackBranch         string

	HistoryID  int64     // id of the newest status.history entry (a successful sync)
	DeployedAt time.Time // its deployedAt
	Phase      string    // status.operationState.phase of the last sync operation
	FinishedAt time.Time // its finishedAt
	Health     string    // status.health.status
	Sync       string    // status.sync.status: Synced when the live state equals the desired state
}

// Parse converts an Application. An Application is tracked when an ImageUpdater CR in iu manages
// it, or when it has the annotation <prefix>/enabled: "true"; "false" opts it out. It returns
// (nil, nil) when the Application is not tracked or has no deployable image yet, and an error
// when its annotations are invalid. iu may be nil.
func Parse(u *unstructured.Unstructured, cfg *config.Config, iu *Store) (*App, error) {
	ann := u.GetAnnotations()
	get := func(k string) string { return ann[cfg.AnnotationPrefix+"/"+k] }
	if get("enabled") == "false" || (cfg.AppFilter != nil && !cfg.AppFilter.MatchString(u.GetName())) {
		return nil, nil
	}
	want := get("image")
	src, matched := iu.Match(u.GetName(), want)
	if get("enabled") != "true" && !matched {
		return nil, nil
	}
	if want == "" {
		want = src.Image
	}

	re := cfg.TagRegex
	if v := get("tag-regex"); v != "" {
		var err error
		if re, err = config.CompileTagRegex(v); err != nil {
			return nil, fmt.Errorf("%s/tag-regex: %w", cfg.AnnotationPrefix, err)
		}
	}

	images, _, _ := unstructured.NestedStringSlice(u.Object, "status", "summary", "images")
	image, imgTag := pickImage(images, want)
	if image == "" {
		return nil, nil
	}
	// summary.images is built from the live resource tree, pods included. During a rolling update
	// the old and the new tag are both listed, so the live tag is unknown until one remains.
	if tags := tagsOf(images, image); len(tags) > 1 {
		imgTag = ""
	}

	owner, name, ok := tag.GitHub(image)
	if v := get("repo"); v != "" {
		owner, name, ok = tag.GitHub(v)
	}
	if !ok {
		return nil, fmt.Errorf("cannot derive GitHub repo from image %q; set the %s/repo annotation", image, cfg.AnnotationPrefix)
	}

	a := &App{
		Name: u.GetName(), Alias: src.Alias, Owner: owner, Repo: name, Image: image, Tag: imgTag,
		Ref:             tag.Ref(re, imgTag),
		DefaultBranch:   orDefault(get("default-branch"), cfg.DefaultBranch),
		PackageWorkflow: orDefault(get("package-workflow"), cfg.PackageWorkflow),
	}

	if o, n, ok := tag.GitHubURL(nestedString(u, "spec", "source", "repoURL")); ok {
		a.GitOpsOwner, a.GitOpsRepo = o, n
		a.WriteBackFile = src.WriteBackFile(nestedString(u, "spec", "source", "path"))
		a.WriteBackBranch = orDefault(src.WriteBackBranch, nestedString(u, "spec", "source", "targetRevision"))
	}

	hist, _, _ := unstructured.NestedSlice(u.Object, "status", "history")
	for _, h := range hist {
		m, _ := h.(map[string]any)
		if id := toInt(m["id"]); id >= a.HistoryID {
			a.HistoryID = id
			a.DeployedAt = parseTime(m["deployedAt"])
		}
	}
	a.Phase, _, _ = unstructured.NestedString(u.Object, "status", "operationState", "phase")
	fin, _, _ := unstructured.NestedString(u.Object, "status", "operationState", "finishedAt")
	a.FinishedAt = parseTime(fin)
	a.Health, _, _ = unstructured.NestedString(u.Object, "status", "health", "status")
	a.Sync, _, _ = unstructured.NestedString(u.Object, "status", "sync", "status")
	return a, nil
}

// pickImage returns the tracked image path and tag. want, if set, selects by image path;
// otherwise the first image wins.
func pickImage(images []string, want string) (path, imgTag string) {
	wantPath, _ := tag.Split(want)
	for _, img := range images {
		p, t := tag.Split(img)
		if want == "" || p == wantPath {
			return p, t
		}
	}
	return "", ""
}

func nestedString(u *unstructured.Unstructured, fields ...string) string {
	v, _, _ := unstructured.NestedString(u.Object, fields...)
	return v
}

// tagsOf returns the distinct tags images lists for the image path.
func tagsOf(images []string, imagePath string) []string {
	var tags []string
	seen := map[string]bool{}
	for _, img := range images {
		if p, t := tag.Split(img); p == imagePath && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	return tags
}

func orDefault(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	}
	return 0
}

func parseTime(v any) time.Time {
	s, _ := v.(string)
	t, _ := time.Parse(time.RFC3339, strings.TrimSpace(s))
	return t
}
