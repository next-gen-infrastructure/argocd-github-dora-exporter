package argocd

import (
	"regexp"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/config"
)

func cfg() *config.Config {
	return &config.Config{
		AnnotationPrefix: "dora-exporter",
		DefaultBranch:    "dev",
		PackageWorkflow:  "build.yml",
		TagRegex:         regexp.MustCompile(`^(?:dev|sha)-(?P<ref>[0-9a-f]{7,40})`),
	}
}

func app(ann map[string]any, images []any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "ns-worker", "annotations": ann},
		"status": map[string]any{
			"summary": map[string]any{"images": images},
			"history": []any{
				map[string]any{"id": int64(3), "deployedAt": "2026-11-19T02:00:00Z"},
				map[string]any{"id": int64(5), "deployedAt": "2026-11-20T02:00:00Z"},
				map[string]any{"id": int64(4), "deployedAt": "2026-11-19T12:00:00Z"},
			},
			"operationState": map[string]any{"phase": "Succeeded", "finishedAt": "2026-11-20T02:00:05Z"},
			"health":         map[string]any{"status": "Healthy"},
		},
	}}
}

func TestParse(t *testing.T) {
	on := map[string]any{"dora-exporter/enabled": "true"}
	imgs := []any{"ghcr.io/acme/app:dev-3ef1767-34f88ad", "ghcr.io/acme/sidecar:1.0"}

	a, err := Parse(app(on, imgs), cfg(), nil)
	if err != nil || a == nil {
		t.Fatalf("Parse = %v, %v", a, err)
	}
	if a.Owner != "acme" || a.Repo != "app" || a.Ref != "3ef1767" || a.Tag != "dev-3ef1767-34f88ad" ||
		a.DefaultBranch != "dev" || a.PackageWorkflow != "" || a.WorkflowDefault != "build.yml" || a.RepoExplicit || a.Health != "Healthy" ||
		a.Phase != "Succeeded" {
		t.Fatalf("unexpected app: %+v", a)
	}
	if a.HistoryID != 5 || !a.DeployedAt.Equal(time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)) {
		t.Fatalf("newest history not picked: %+v", a)
	}
}

func TestParseOverrides(t *testing.T) {
	a, err := Parse(app(map[string]any{
		"dora-exporter/enabled":          "true",
		"dora-exporter/image":            "ghcr.io/acme/sidecar",
		"dora-exporter/repo":             "other/repo",
		"dora-exporter/package-workflow": "pkg.yml",
		"dora-exporter/default-branch":   "main",
		"dora-exporter/tag-regex":        `^(?P<ref>[0-9.]+)$`,
	}, []any{"ghcr.io/acme/app:dev-3ef1767", "ghcr.io/acme/sidecar:1.0"}), cfg(), nil)
	if err != nil || a == nil {
		t.Fatalf("Parse = %v, %v", a, err)
	}
	if a.Owner != "other" || a.Repo != "repo" || a.Image != "acme/sidecar" || a.Ref != "1.0" ||
		a.PackageWorkflow != "pkg.yml" || a.WorkflowDefault != "build.yml" || !a.RepoExplicit || a.DefaultBranch != "main" {
		t.Fatalf("unexpected app: %+v", a)
	}
}

func TestParseSkipsAndErrors(t *testing.T) {
	imgs := []any{"ghcr.io/acme/app:dev-3ef1767"}
	if a, err := Parse(app(nil, imgs), cfg(), nil); a != nil || err != nil {
		t.Errorf("not opted in: got %v, %v", a, err)
	}
	if a, err := Parse(app(map[string]any{"dora-exporter/enabled": "true"}, nil), cfg(), nil); a != nil || err != nil {
		t.Errorf("no images: got %v, %v", a, err)
	}
	if _, err := Parse(app(map[string]any{"dora-exporter/enabled": "true"}, []any{"ghcr.io/acme/sub/app:x"}), cfg(), nil); err == nil {
		t.Error("underivable repo should error")
	}
	if _, err := Parse(app(map[string]any{"dora-exporter/enabled": "true", "dora-exporter/tag-regex": "("}, imgs), cfg(), nil); err == nil {
		t.Error("bad tag-regex should error")
	}
}

func imageUpdater(name, pattern, image, alias string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": name},
		"spec": map[string]any{"applicationRefs": []any{map[string]any{
			"namePattern": pattern,
			"images":      []any{map[string]any{"alias": alias, "imageName": image}},
		}}},
	}}
}

func TestParseTracksApplicationsFromImageUpdater(t *testing.T) {
	iu := NewStore()
	iu.Set(imageUpdater("agents", "ns-*", "ghcr.io/acme/app", "ns-worker"))
	// Prod-like: an unrelated image is listed first; the ImageUpdater image must win.
	imgs := []any{"ghcr.io/other/otel:0.66b1", "ghcr.io/acme/app:sha-2343648"}

	a, err := Parse(app(nil, imgs), cfg(), iu) // no annotation at all
	if err != nil || a == nil {
		t.Fatalf("Parse = %v, %v", a, err)
	}
	if a.Owner != "acme" || a.Repo != "app" || a.Alias != "ns-worker" || a.Ref != "2343648" {
		t.Fatalf("unexpected app: %+v", a)
	}

	// Explicit opt-out wins over the ImageUpdater match.
	if a, _ := Parse(app(map[string]any{"dora-exporter/enabled": "false"}, imgs), cfg(), iu); a != nil {
		t.Error("enabled=false should opt out")
	}
	// An Application no ImageUpdater CR matches stays untracked.
	u := app(nil, imgs)
	u.SetName("unrelated")
	if a, _ := Parse(u, cfg(), iu); a != nil {
		t.Error("unmatched application should not be tracked")
	}
	// Deleting the CR stops the matching.
	iu.Delete("agents")
	if a, _ := Parse(app(nil, imgs), cfg(), iu); a != nil {
		t.Error("deleted ImageUpdater CR still matches")
	}
}

func TestParseGitOpsWriteBack(t *testing.T) {
	cr := imageUpdater("agents", "ns-worker", "ghcr.io/acme/app", "ns-worker")
	cr.Object["spec"].(map[string]any)["writeBackConfig"] = map[string]any{
		"method": "git",
		"gitConfig": map[string]any{
			"branch":          "dev",
			"writeBackTarget": "helmvalues:./values-dev.yaml",
		},
	}
	iu := NewStore()
	iu.Set(cr)

	u := app(nil, []any{"ghcr.io/acme/app:dev-3ef1767"})
	u.Object["spec"] = map[string]any{"source": map[string]any{
		"repoURL": "https://github.com/acme/gitops", "path": "namespaces/agents/worker", "targetRevision": "main",
	}}
	a, err := Parse(u, cfg(), iu)
	if err != nil || a == nil {
		t.Fatalf("Parse = %v, %v", a, err)
	}
	if a.GitOpsOwner != "acme" || a.GitOpsRepo != "gitops" ||
		a.WriteBackFile != "namespaces/agents/worker/values-dev.yaml" || a.WriteBackBranch != "dev" {
		t.Fatalf("unexpected gitops fields: %+v", a)
	}

	// Without a CR write-back config the file is unknown and the branch falls back to targetRevision.
	iu2 := NewStore()
	iu2.Set(imageUpdater("agents", "ns-worker", "ghcr.io/acme/app", "ns-worker"))
	a, _ = Parse(u, cfg(), iu2)
	if a == nil || a.WriteBackFile != "" || a.WriteBackBranch != "main" {
		t.Fatalf("unexpected fallback: %+v", a)
	}
}

func TestWriteBackFile(t *testing.T) {
	for target, want := range map[string]string{
		"helmvalues:./values-dev.yaml": "ns/app/values-dev.yaml",
		"helmvalues:values-dev.yaml":   "ns/app/values-dev.yaml",
		"helmvalues:/ns/other/v.yaml":  "ns/other/v.yaml",
		"kustomization":                "",
		"":                             "",
	} {
		if got := (Source{WriteBackTarget: target}).WriteBackFile("ns/app"); got != want {
			t.Errorf("WriteBackFile(%q) = %q, want %q", target, got, want)
		}
	}
}

func TestParseAppFilter(t *testing.T) {
	c := cfg()
	c.AppFilter = regexp.MustCompile(`^other$`)
	on := map[string]any{"dora-exporter/enabled": "true"}
	if a, _ := Parse(app(on, []any{"ghcr.io/acme/app:dev-3ef1767"}), c, nil); a != nil {
		t.Error("filtered-out application should not be tracked")
	}
	c.AppFilter = regexp.MustCompile(`^ns-worker$`)
	if a, _ := Parse(app(on, []any{"ghcr.io/acme/app:dev-3ef1767"}), c, nil); a == nil {
		t.Error("matching application should be tracked")
	}
}

func TestParseRollingUpdateHasNoLiveTag(t *testing.T) {
	on := map[string]any{"dora-exporter/enabled": "true"}
	// Old and new pods both run: two tags of the tracked image are listed.
	a, err := Parse(app(on, []any{"ghcr.io/acme/app:dev-3ef1767", "ghcr.io/acme/app:dev-aaaaaaa"}), cfg(), nil)
	if err != nil || a == nil {
		t.Fatalf("Parse = %v, %v", a, err)
	}
	if a.Tag != "" || a.Ref != "" {
		t.Fatalf("ambiguous tags must leave the ref unknown: %+v", a)
	}
	// The same tag listed twice (two workloads) is not ambiguous.
	a, _ = Parse(app(on, []any{"ghcr.io/acme/app:dev-3ef1767", "ghcr.io/acme/app:dev-3ef1767"}), cfg(), nil)
	if a == nil || a.Ref != "3ef1767" {
		t.Fatalf("duplicate tag treated as ambiguous: %+v", a)
	}
}
