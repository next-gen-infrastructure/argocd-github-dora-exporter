// Command argocd-github-dora-exporter exposes DORA metrics derived from Argo CD and GitHub.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/builds"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/config"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/deploy"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/ghclient"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/leadtime"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/prs"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/tracked"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}
	rc, err := kubeConfig()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	m := metrics.New(cfg.Environment)
	var gh *ghclient.Client
	if cfg.GitHubToken != "" {
		gh = ghclient.NewWithToken(cfg.GitHubToken, m)
	} else if gh, err = ghclient.New(cfg.GitHubAppID, cfg.GitHubInstallationID, cfg.GitHubPrivateKey, m); err != nil {
		return err
	}
	if cfg.GitHubAPIURL != "" {
		gh.SetBaseURL(cfg.GitHubAPIURL)
	}
	repos := tracked.New()
	iu := argocd.NewStore()
	lead := leadtime.New(gh, m, log)
	tracker := deploy.New(m, time.Now, lead.Submit)
	go lead.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Addr: cfg.ListenAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "error", err)
			stop()
		}
	}()

	handle := func(a *argocd.App) {
		repos.Add(tracked.Repo{Owner: a.Owner, Name: a.Repo, DefaultBranch: a.DefaultBranch, PackageWorkflow: a.PackageWorkflow})
		tracker.Observe(a)
	}
	err = argocd.Watch(ctx, rc, cfg, log, iu, handle, func() {
		log.Info("argocd applications synced", "namespace", cfg.ArgoCDNamespace)
		// ponytail: marks liveness of the watch from the initial sync on; the informer
		// reconnects by itself, so a stale value only appears if this process hangs.
		go func() {
			for t := time.NewTicker(30 * time.Second); ; <-t.C {
				m.LastPoll.WithLabelValues("argocd").SetToCurrentTime()
				if ctx.Err() != nil {
					return
				}
			}
		}()
		go pollGitHub(ctx, log, cfg, m, gh, repos)
	})

	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	return err
}

// pollGitHub runs the PR and build pollers every PollInterval. Both only observe what happens
// after this call, so nothing that was already counted before a restart is counted again.
func pollGitHub(ctx context.Context, log *slog.Logger, cfg *config.Config, m *metrics.Metrics,
	gh *ghclient.Client, repos *tracked.Set) {
	started := time.Now().Add(-cfg.InitialLookback)
	prPoller := prs.New(gh, m, log, started, cfg.ChecksInclude, cfg.FirstReview)
	buildPoller := builds.New(gh, m, started)

	timer := time.NewTimer(10 * time.Second) // let the Application cache settle first
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		list := repos.List()
		err := errors.Join(prPoller.Poll(ctx, list), buildPoller.Poll(ctx, list))
		if err != nil {
			// One line per poll: the joined error lists every failing repo, and usually the cause is shared.
			msg := err.Error()
			first, _, _ := strings.Cut(msg, "\n")
			log.Warn("github poll failed", "failures", strings.Count(msg, "\n")+1, "first", first)
		} else {
			m.LastPoll.WithLabelValues("github").SetToCurrentTime()
		}
		timer.Reset(cfg.PollInterval)
	}
}

// kubeConfig uses the in-cluster config and falls back to the local kubeconfig (KUBECONFIG).
func kubeConfig() (*rest.Config, error) {
	if c, err := rest.InClusterConfig(); err == nil {
		return c, nil
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{}).ClientConfig()
}
