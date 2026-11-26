// Package deploy turns Application observations into deployment, failure and restore metrics.
package deploy

import (
	"sync"
	"time"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

// degradedWindow is how long after a successful sync a Degraded health still counts as a
// failure of that sync. ponytail: fixed 30m, make it configurable if it needs tuning.
const degradedWindow = 30 * time.Minute

// Deployed describes a new image ref reaching an application.
type Deployed struct {
	App     argocd.App
	PrevRef string
	At      time.Time // deployedAt of the sync that delivered it
}

// Tracker keeps per-application state between observations.
type Tracker struct {
	m        *metrics.Metrics
	now      func() time.Time
	onDeploy func(Deployed)
	mu       sync.Mutex
	apps     map[string]*state
}

type state struct {
	historyID        int64     // newest successful sync already counted
	lastFailedFinish time.Time // finishedAt of the last failed sync already counted
	degradedFor      int64     // history id whose Degraded failure was already counted
	failedSince      time.Time // start of an unresolved failure; zero when none
	awaitingHealthy  bool      // a successful sync happened after the failure
	ref              string    // image ref last known to be live; empty if unknown
}

// New returns a Tracker. now is time.Now in production. onDeploy, if set, is called (while the
// tracker lock is held, so it must not block) when an application gets a new image ref.
func New(m *metrics.Metrics, now func() time.Time, onDeploy func(Deployed)) *Tracker {
	return &Tracker{m: m, now: now, onDeploy: onDeploy, apps: map[string]*state{}}
}

// Observe records the current state of an application. The first observation of an
// application only sets the baseline: nothing that already happened is counted, because it
// was counted by a previous process or is unknown.
// ponytail: events during downtime are lost; persist a watermark if that matters.
func (t *Tracker) Observe(a *argocd.App) {
	t.mu.Lock()
	defer t.mu.Unlock()

	s, known := t.apps[a.Name]
	if !known {
		s = &state{historyID: a.HistoryID, lastFailedFinish: a.FinishedAt, degradedFor: a.HistoryID}
		// status.summary.images is the desired state; it is only the live ref when Synced.
		// ponytail: an app that is OutOfSync at start has an unknown live ref, so its next
		// deployment is not attributed. Recover it from the image updater message if that hurts.
		if a.Sync == "Synced" {
			s.ref = a.Ref
		}
		t.apps[a.Name] = s
		return
	}

	// A new history entry is a successful sync.
	if a.HistoryID > s.historyID {
		s.historyID = a.HistoryID
		t.m.Deployments.WithLabelValues(a.Repo, a.Name, "success").Inc()
		t.m.LastDeployment.WithLabelValues(a.Repo, a.Name).Set(float64(a.DeployedAt.Unix()))
		if !s.failedSince.IsZero() {
			s.awaitingHealthy = true
		}
	}

	// A changed image ref is a new set of commits reaching the environment. status.summary.images
	// is the desired state: it changes when Argo CD resolves the new commit, before the sync. Only
	// when the app is Synced is that ref live, and the newest history entry is the sync that put it
	// there.
	if a.Sync == "Synced" && a.Ref != "" && a.Ref != s.ref {
		prev := s.ref
		s.ref = a.Ref
		if prev != "" && t.onDeploy != nil {
			at := a.DeployedAt
			if at.IsZero() {
				at = t.now()
			}
			t.onDeploy(Deployed{App: *a, PrevRef: prev, At: at})
		}
	}

	// A failed sync never enters history; count it by its finish time.
	if (a.Phase == "Failed" || a.Phase == "Error") && a.FinishedAt.After(s.lastFailedFinish) {
		s.lastFailedFinish = a.FinishedAt
		t.failure(s, a, a.FinishedAt)
	}

	// A successful sync that turns Degraded shortly after counts as failed, once.
	if a.Health == "Degraded" && s.degradedFor != s.historyID && !a.DeployedAt.IsZero() &&
		t.now().Sub(a.DeployedAt) <= degradedWindow {
		s.degradedFor = s.historyID
		t.failure(s, a, t.now())
	}

	// Restore: the first healthy state after a sync that followed the failure.
	if s.awaitingHealthy && a.Health == "Healthy" {
		t.m.RestoreTime.WithLabelValues(a.Repo, a.Name).Observe(t.now().Sub(s.failedSince).Seconds())
		s.failedSince, s.awaitingHealthy = time.Time{}, false
	}
}

func (t *Tracker) failure(s *state, a *argocd.App, at time.Time) {
	t.m.Deployments.WithLabelValues(a.Repo, a.Name, "failure").Inc()
	if s.failedSince.IsZero() {
		s.failedSince = at
	}
}
