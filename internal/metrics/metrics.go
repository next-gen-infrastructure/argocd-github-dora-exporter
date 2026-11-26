// Package metrics defines the Prometheus collectors listed in docs/contract.md.
package metrics

import "github.com/prometheus/client_golang/prometheus"

var (
	// LongBuckets suit lead time, restore time and PR stages (1m to 30d).
	LongBuckets = []float64{60, 300, 900, 1800, 3600, 7200, 14400, 28800, 86400,
		172800, 345600, 604800, 1209600, 2592000}
	// ShortBuckets suit checks, builds, queues, rollout and merge delay (5s to 2h).
	ShortBuckets = []float64{5, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200}
	// CountBuckets suit small counts such as commits per PR.
	CountBuckets = []float64{1, 2, 3, 5, 8, 13, 21}
)

// Metrics holds all collectors. The `environment` label is a constant label on each.
type Metrics struct {
	Registry *prometheus.Registry

	Deployments    *prometheus.CounterVec   // repo, app, result
	LastDeployment *prometheus.GaugeVec     // repo, app
	LeadTime       *prometheus.HistogramVec // repo
	RestoreTime    *prometheus.HistogramVec // repo, app
	Rollout        *prometheus.HistogramVec // repo, app
	ImageUpdate    *prometheus.HistogramVec // repo, app

	PRStage         *prometheus.HistogramVec // repo, base, stage
	PRFirstReview   *prometheus.HistogramVec // repo, base
	PRChecks        *prometheus.HistogramVec // repo, base
	PRCheckQueue    *prometheus.HistogramVec // repo, base
	PRCheckDuration *prometheus.HistogramVec // repo, base, check, conclusion
	PRCheckRuns     *prometheus.CounterVec   // repo, base, check, conclusion
	PRMergeDelay    *prometheus.HistogramVec // repo, base
	PRHeadCommits   *prometheus.HistogramVec // repo, base

	BuildQueue    *prometheus.HistogramVec // repo, workflow, conclusion
	BuildDuration *prometheus.HistogramVec // repo, workflow, conclusion

	GitHubRequests     *prometheus.CounterVec // api, code
	RateLimitRemaining *prometheus.GaugeVec   // api
	LastPoll           *prometheus.GaugeVec   // source
}

// New creates and registers the collectors.
func New(environment string) *Metrics {
	cl := prometheus.Labels{"environment": environment}
	hist := func(name, help string, buckets []float64, labels ...string) *prometheus.HistogramVec {
		return prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: name, Help: help, Buckets: buckets, ConstLabels: cl}, labels)
	}
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		Deployments: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dora_deployments_total", Help: "Deployments by result (success or failure).",
			ConstLabels: cl}, []string{"repo", "app", "result"}),
		LastDeployment: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dora_last_deployment_timestamp_seconds", Help: "Time of the last successful deployment.",
			ConstLabels: cl}, []string{"repo", "app"}),
		LeadTime: hist("dora_lead_time_seconds",
			"Per commit: time from commit to deployment.", LongBuckets, "repo"),
		RestoreTime: hist("dora_restore_time_seconds",
			"Time from a failed deployment to the next healthy sync.", LongBuckets, "repo", "app"),
		Rollout: hist("dora_rollout_seconds",
			"Time from the package workflow finishing to the Argo CD sync.", ShortBuckets, "repo", "app"),

		ImageUpdate: hist("dora_image_update_seconds",
			"Package workflow finishing to image updater writing the new tag back.", ShortBuckets, "repo", "app"),

		PRStage: hist("dora_pr_stage_seconds",
			"Merged PR stages: coding, draft, review.", LongBuckets, "repo", "base", "stage"),
		PRFirstReview: hist("dora_pr_first_review_seconds",
			"Review start to the first review by someone other than the author.", LongBuckets, "repo", "base"),
		PRChecks: hist("dora_pr_checks_seconds",
			"Earliest check start to latest check completion on the final head commit.", ShortBuckets, "repo", "base"),
		PRCheckQueue: hist("dora_pr_check_queue_seconds",
			"Head commit date to the first check start.", ShortBuckets, "repo", "base"),
		PRCheckDuration: hist("dora_pr_check_duration_seconds",
			"Duration of a single check run.", ShortBuckets, "repo", "base", "check", "conclusion"),
		PRCheckRuns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dora_pr_check_runs_total", Help: "Check runs on the final head commit of merged PRs.",
			ConstLabels: cl}, []string{"repo", "base", "check", "conclusion"}),
		PRMergeDelay: hist("dora_pr_merge_delay_seconds",
			"Latest check completion to merge.", ShortBuckets, "repo", "base"),
		PRHeadCommits: hist("dora_pr_head_commits",
			"Commits made after the review start (approximates pushes), at least 1.", CountBuckets, "repo", "base"),

		BuildQueue: hist("dora_build_queue_seconds",
			"Package workflow job: created to started (runner wait).", ShortBuckets, "repo", "workflow", "conclusion"),
		BuildDuration: hist("dora_build_duration_seconds",
			"Package workflow job: started to completed.", ShortBuckets, "repo", "workflow", "conclusion"),

		GitHubRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dora_exporter_github_requests_total", Help: "GitHub API requests by api and status code.",
			ConstLabels: cl}, []string{"api", "code"}),
		RateLimitRemaining: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dora_exporter_github_rate_limit_remaining", Help: "Remaining GitHub API quota.",
			ConstLabels: cl}, []string{"api"}),
		LastPoll: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "dora_exporter_last_poll_timestamp_seconds", Help: "Last time a source was successfully read.",
			ConstLabels: cl}, []string{"source"}),
	}
	m.Registry.MustRegister(m.Deployments, m.LastDeployment, m.LeadTime, m.RestoreTime, m.Rollout, m.ImageUpdate,
		m.PRStage, m.PRFirstReview, m.PRChecks, m.PRCheckQueue, m.PRCheckDuration, m.PRCheckRuns,
		m.PRMergeDelay, m.PRHeadCommits, m.BuildQueue, m.BuildDuration,
		m.GitHubRequests, m.RateLimitRemaining, m.LastPoll)
	return m
}
