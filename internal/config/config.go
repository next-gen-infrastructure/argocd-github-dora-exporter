// Package config loads the exporter configuration from environment variables
// (see docs/contract.md).
package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTagRegex = `^(?:dev|sha)-(?P<ref>[0-9a-f]{7,40})`
	// RefGroup is the required named group of the tag regex.
	RefGroup = "ref"
)

// Config is the effective exporter configuration.
type Config struct {
	GitHubAppID          string
	GitHubInstallationID string
	GitHubPrivateKey     string
	GitHubToken          string // static token; replaces the App credentials when set
	GitHubAPIURL         string
	Environment          string
	ArgoCDNamespace      string
	AnnotationPrefix     string
	DefaultBranch        string
	TagRegex             *regexp.Regexp
	PackageWorkflow      string
	ChecksInclude        *regexp.Regexp
	AppFilter            *regexp.Regexp // only Applications whose name matches are tracked; nil means all
	FirstReview          bool
	PollInterval         time.Duration
	InitialLookback      time.Duration // how far back the first poll looks; 0 means nothing is backfilled
	ListenAddr           string
}

// Load reads the configuration using getenv (os.Getenv in production).
// All problems are reported together.
func Load(getenv func(string) string) (*Config, error) {
	var errs []error
	req := func(key string) string {
		v := getenv(key)
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", key))
		}
		return v
	}
	def := func(key, d string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return d
	}

	token := getenv("GITHUB_TOKEN")
	if f := getenv("GITHUB_TOKEN_FILE"); token == "" && f != "" {
		b, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("GITHUB_TOKEN_FILE: %w", err))
		}
		token = strings.TrimSpace(string(b))
	}
	// App credentials are only required without a static token.
	appReq := req
	if token != "" {
		appReq = getenv
	}

	c := &Config{
		GitHubToken:          token,
		GitHubAppID:          appReq("GITHUB_APP_ID"),
		GitHubInstallationID: appReq("GITHUB_APP_INSTALLATION_ID"),
		Environment:          req("ENVIRONMENT"),
		DefaultBranch:        req("DEFAULT_BRANCH"),
		ArgoCDNamespace:      def("ARGOCD_NAMESPACE", "argocd"),
		AnnotationPrefix:     def("ANNOTATION_PREFIX", "dora-exporter"),
		PackageWorkflow:      getenv("PACKAGE_WORKFLOW"),
		ListenAddr:           def("LISTEN_ADDR", ":9090"),
		GitHubAPIURL:         def("GITHUB_API_URL", ""),
	}

	c.GitHubPrivateKey = getenv("GITHUB_APP_PRIVATE_KEY")
	if c.GitHubPrivateKey == "" && token == "" {
		if f := getenv("GITHUB_APP_PRIVATE_KEY_FILE"); f != "" {
			b, err := os.ReadFile(f)
			if err != nil {
				errs = append(errs, fmt.Errorf("GITHUB_APP_PRIVATE_KEY_FILE: %w", err))
			}
			c.GitHubPrivateKey = string(b)
		} else {
			errs = append(errs, errors.New("GITHUB_APP_PRIVATE_KEY or GITHUB_APP_PRIVATE_KEY_FILE is required"))
		}
	}

	if r := def("TAG_RESOLVER", "sha"); r != "sha" {
		errs = append(errs, fmt.Errorf("TAG_RESOLVER %q is not supported (only \"sha\")", r))
	}

	re, err := CompileTagRegex(def("TAG_REGEX", defaultTagRegex))
	if err != nil {
		errs = append(errs, fmt.Errorf("TAG_REGEX: %w", err))
	}
	c.TagRegex = re

	if c.ChecksInclude, err = regexp.Compile(def("CHECKS_INCLUDE", ".*")); err != nil {
		errs = append(errs, fmt.Errorf("CHECKS_INCLUDE: %w", err))
	}

	if v := getenv("APP_FILTER"); v != "" {
		if c.AppFilter, err = regexp.Compile(v); err != nil {
			errs = append(errs, fmt.Errorf("APP_FILTER: %w", err))
		}
	}

	if v := getenv("FIRST_REVIEW"); v != "" {
		if c.FirstReview, err = strconv.ParseBool(v); err != nil {
			errs = append(errs, fmt.Errorf("FIRST_REVIEW: %w", err))
		}
	}

	if c.PollInterval, err = time.ParseDuration(def("POLL_INTERVAL", "5m")); err != nil {
		errs = append(errs, fmt.Errorf("POLL_INTERVAL: %w", err))
	} else if c.PollInterval < time.Minute {
		errs = append(errs, errors.New("POLL_INTERVAL must be at least 1m"))
	}

	if v := getenv("INITIAL_LOOKBACK"); v != "" {
		if c.InitialLookback, err = time.ParseDuration(v); err != nil || c.InitialLookback < 0 {
			errs = append(errs, fmt.Errorf("INITIAL_LOOKBACK %q is not a non-negative duration", v))
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

// CompileTagRegex compiles a tag regex and checks it has the named group "ref".
func CompileTagRegex(expr string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(strings.TrimSpace(expr))
	if err != nil {
		return nil, err
	}
	if re.SubexpIndex(RefGroup) < 0 {
		return nil, fmt.Errorf("must contain the named group (?P<%s>...)", RefGroup)
	}
	return re, nil
}
