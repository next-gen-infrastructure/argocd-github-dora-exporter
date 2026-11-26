package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func valid() map[string]string {
	return map[string]string{
		"GITHUB_APP_ID":              "1",
		"GITHUB_APP_INSTALLATION_ID": "2",
		"GITHUB_APP_PRIVATE_KEY":     "pem",
		"ENVIRONMENT":                "dev",
		"DEFAULT_BRANCH":             "dev",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(valid()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ArgoCDNamespace != "argocd" || c.AnnotationPrefix != "dora-exporter" ||
		c.PollInterval != 5*time.Minute || c.ListenAddr != ":9090" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if got := c.TagRegex.FindStringSubmatch("sha-abcdef1")[c.TagRegex.SubexpIndex("ref")]; got != "abcdef1" {
		t.Fatalf("default regex ref = %q", got)
	}
}

func TestLoadErrors(t *testing.T) {
	m := valid()
	delete(m, "ENVIRONMENT")
	delete(m, "GITHUB_APP_PRIVATE_KEY")
	m["TAG_RESOLVER"] = "semver"
	m["TAG_REGEX"] = `^dev-(?P<sha>[0-9a-f]+)`
	m["POLL_INTERVAL"] = "10s"
	_, err := Load(env(m))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"ENVIRONMENT is required", "GITHUB_APP_PRIVATE_KEY", "TAG_RESOLVER", "named group", "at least 1m"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestLoadStaticTokenReplacesAppCredentials(t *testing.T) {
	m := map[string]string{"GITHUB_TOKEN": "static-token", "ENVIRONMENT": "dev", "DEFAULT_BRANCH": "dev"}
	c, err := Load(env(m))
	if err != nil || c.GitHubToken != "static-token" {
		t.Fatalf("Load = %+v, %v", c, err)
	}
	delete(m, "GITHUB_TOKEN")
	if _, err := Load(env(m)); err == nil {
		t.Fatal("expected an error without token or app credentials")
	}
}
