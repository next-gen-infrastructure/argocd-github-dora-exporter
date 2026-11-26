// Package tag extracts commit refs from image tags and maps image paths to GitHub repositories.
package tag

import (
	"regexp"
	"strings"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/config"
)

// Ref returns the "ref" group of tag matched by re, or "" if it does not match.
func Ref(re *regexp.Regexp, tag string) string {
	m := re.FindStringSubmatch(tag)
	i := re.SubexpIndex(config.RefGroup)
	if m == nil || i < 0 {
		return ""
	}
	return m[i]
}

// Split splits "[registry/]owner/name[:tag][@digest]" into the repository path
// without the registry host, and the tag.
func Split(image string) (repo, tag string) {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		image, tag = image[:i], image[i+1:]
	}
	if host, rest, ok := strings.Cut(image, "/"); ok &&
		(strings.ContainsAny(host, ".:") || host == "localhost") {
		image = rest
	}
	return image, tag
}

// GitHub maps a repository path to its GitHub owner and name. It only accepts exactly
// "owner/name"; anything else needs an explicit repo annotation.
func GitHub(repo string) (owner, name string, ok bool) {
	owner, name, ok = strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return owner, name, true
}

// GitHubURL extracts owner and name from an https or ssh GitHub repository URL
// (https://github.com/o/n[.git], git@github.com:o/n[.git]).
func GitHubURL(u string) (owner, name string, ok bool) {
	u = strings.TrimSuffix(strings.TrimSpace(u), ".git")
	switch {
	case strings.HasPrefix(u, "git@"):
		_, u, _ = strings.Cut(u, ":")
	case strings.Contains(u, "://"):
		_, u, _ = strings.Cut(u, "://")
		_, u, _ = strings.Cut(u, "/") // drop the host
	default:
		return "", "", false
	}
	return GitHub(u)
}
