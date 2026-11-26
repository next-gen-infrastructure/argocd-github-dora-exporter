package tag

import (
	"regexp"
	"testing"
)

func TestRef(t *testing.T) {
	re := regexp.MustCompile(`^(?:dev|sha)-(?P<ref>[0-9a-f]{7,40})`)
	for tag, want := range map[string]string{
		"dev-3ef1767":         "3ef1767",
		"dev-3ef1767-34f88ad": "3ef1767", // second sha ignored
		"sha-2343648":         "2343648",
		"latest":              "",
		"dev-xyz":             "",
		"":                    "",
	} {
		if got := Ref(re, tag); got != want {
			t.Errorf("Ref(%q) = %q, want %q", tag, got, want)
		}
	}
}

func TestSplit(t *testing.T) {
	for _, c := range []struct{ in, repo, tag string }{
		{"ghcr.io/acme/app:dev-abc1234", "acme/app", "dev-abc1234"},
		{"acme/app:sha-abc1234", "acme/app", "sha-abc1234"},
		{"acme/app", "acme/app", ""},
		{"registry:5000/acme/app:v1", "acme/app", "v1"},
		{"ghcr.io/acme/app:v1@sha256:deadbeef", "acme/app", "v1"},
		{"localhost/acme/app:v1", "acme/app", "v1"},
		{"ghcr.io/acme/sub/app:v1", "acme/sub/app", "v1"},
	} {
		repo, tag := Split(c.in)
		if repo != c.repo || tag != c.tag {
			t.Errorf("Split(%q) = (%q, %q), want (%q, %q)", c.in, repo, tag, c.repo, c.tag)
		}
	}
}

func TestGitHub(t *testing.T) {
	if o, n, ok := GitHub("acme/app"); !ok || o != "acme" || n != "app" {
		t.Errorf("GitHub(acme/app) = %q %q %v", o, n, ok)
	}
	for _, bad := range []string{"app", "acme/sub/app", "/app", "acme/"} {
		if _, _, ok := GitHub(bad); ok {
			t.Errorf("GitHub(%q) should not be ok", bad)
		}
	}
}

func TestGitHubURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/gitops":     "acme/gitops",
		"https://github.com/acme/gitops.git": "acme/gitops",
		"git@github.com:acme/gitops.git":     "acme/gitops",
		"ssh://git@github.com/acme/gitops":   "acme/gitops",
		"acme/gitops":                        "",
		"https://github.com/acme":            "",
	} {
		o, n, ok := GitHubURL(in)
		got := ""
		if ok {
			got = o + "/" + n
		}
		if got != want {
			t.Errorf("GitHubURL(%q) = %q, want %q", in, got, want)
		}
	}
}
