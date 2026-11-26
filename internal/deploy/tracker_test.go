package deploy

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/argocd"
	"github.com/next-gen-infrastructure/argocd-github-dora-exporter/internal/metrics"
)

var t0 = time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)

type harness struct {
	t        *testing.T
	m        *metrics.Metrics
	tr       *Tracker
	now      time.Time
	deployed []Deployed
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, m: metrics.New("test"), now: t0}
	h.tr = New(h.m, func() time.Time { return h.now }, func(d Deployed) { h.deployed = append(h.deployed, d) })
	return h
}

func (h *harness) app(id int64, deployed time.Time, phase string, finished time.Time, health string) *argocd.App {
	return &argocd.App{Name: "ns-app", Repo: "app", HistoryID: id, DeployedAt: deployed,
		Phase: phase, FinishedAt: finished, Health: health, Sync: "Synced"}
}

func (h *harness) count(result string) float64 {
	return testutil.ToFloat64(h.m.Deployments.WithLabelValues("app", "ns-app", result))
}

func (h *harness) restores() int {
	return testutil.CollectAndCount(h.m.RestoreTime)
}

func TestBaselineEmitsNothing(t *testing.T) {
	h := newHarness(t)
	h.tr.Observe(h.app(7, t0.Add(-time.Hour), "Failed", t0.Add(-time.Minute), "Degraded"))
	if h.count("success") != 0 || h.count("failure") != 0 {
		t.Fatal("baseline observation must not count anything")
	}
}

func TestSuccessCountedOnce(t *testing.T) {
	h := newHarness(t)
	h.tr.Observe(h.app(1, t0, "Succeeded", t0, "Healthy"))
	h.now = t0.Add(time.Hour)
	next := h.app(2, h.now, "Succeeded", h.now, "Healthy")
	h.tr.Observe(next)
	h.tr.Observe(next) // resync of the same state
	if got := h.count("success"); got != 1 {
		t.Fatalf("success = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.m.LastDeployment.WithLabelValues("app", "ns-app")); got != float64(h.now.Unix()) {
		t.Fatalf("last deployment = %v", got)
	}
}

func TestFailedSyncCountedOnceThenRestored(t *testing.T) {
	h := newHarness(t)
	h.tr.Observe(h.app(1, t0, "Succeeded", t0, "Healthy"))

	failAt := t0.Add(time.Hour)
	h.now = failAt
	failed := h.app(1, t0, "Failed", failAt, "Degraded")
	h.tr.Observe(failed)
	h.tr.Observe(failed)
	if got := h.count("failure"); got != 1 {
		t.Fatalf("failure = %v, want 1", got)
	}

	// Fixing sync, but health not yet Healthy: no restore yet.
	h.now = failAt.Add(10 * time.Minute)
	h.tr.Observe(h.app(2, h.now, "Succeeded", h.now, "Progressing"))
	if h.restores() != 0 {
		t.Fatal("restore observed before the app is healthy")
	}
	h.now = failAt.Add(15 * time.Minute)
	h.tr.Observe(h.app(2, h.now.Add(-5*time.Minute), "Succeeded", h.now, "Healthy"))
	if h.restores() != 1 {
		t.Fatal("restore not observed")
	}
	if got := testutil.ToFloat64(h.m.Deployments.WithLabelValues("app", "ns-app", "success")); got != 1 {
		t.Fatalf("success = %v, want 1", got)
	}
}

func TestDegradedAfterSuccessCountedOnceWithinWindow(t *testing.T) {
	h := newHarness(t)
	h.tr.Observe(h.app(1, t0, "Succeeded", t0, "Healthy"))

	deployed := t0.Add(time.Hour)
	h.now = deployed
	h.tr.Observe(h.app(2, deployed, "Succeeded", deployed, "Healthy"))

	h.now = deployed.Add(5 * time.Minute)
	deg := h.app(2, deployed, "Succeeded", deployed, "Degraded")
	h.tr.Observe(deg)
	h.tr.Observe(deg)
	if got := h.count("failure"); got != 1 {
		t.Fatalf("failure = %v, want 1", got)
	}

	// Degraded long after the sync is not blamed on it.
	h2 := newHarness(t)
	h2.tr.Observe(h2.app(1, t0, "Succeeded", t0, "Healthy"))
	h2.now = t0.Add(2 * time.Hour)
	h2.tr.Observe(h2.app(1, t0, "Succeeded", t0, "Degraded"))
	if got := h2.count("failure"); got != 0 {
		t.Fatalf("late degraded counted as failure: %v", got)
	}
}

func TestRefChangeEmitsDeployedOnce(t *testing.T) {
	h := newHarness(t)
	withRef := func(ref string, id int64, deployed time.Time) *argocd.App {
		a := h.app(id, deployed, "Succeeded", deployed, "Healthy")
		a.Ref = ref
		return a
	}
	h.tr.Observe(withRef("aaa1111", 1, t0)) // baseline: nothing emitted
	if len(h.deployed) != 0 {
		t.Fatal("baseline emitted a deployment")
	}
	at := t0.Add(time.Hour)
	h.tr.Observe(withRef("bbb2222", 2, at))
	h.tr.Observe(withRef("bbb2222", 2, at)) // resync
	if len(h.deployed) != 1 {
		t.Fatalf("emitted %d deployments, want 1", len(h.deployed))
	}
	if d := h.deployed[0]; d.PrevRef != "aaa1111" || d.App.Ref != "bbb2222" || !d.At.Equal(at) {
		t.Fatalf("unexpected deployment: %+v", d)
	}
	// An unparsable tag (empty ref) never emits and never resets the known ref.
	h.tr.Observe(withRef("", 3, at.Add(time.Hour)))
	h.tr.Observe(withRef("ccc3333", 4, at.Add(2*time.Hour)))
	if len(h.deployed) != 2 || h.deployed[1].PrevRef != "bbb2222" {
		t.Fatalf("unexpected deployments: %+v", h.deployed)
	}
}

func TestRefChangeWaitsForSync(t *testing.T) {
	h := newHarness(t)
	obs := func(ref, sync string, id int64, deployed time.Time) {
		a := h.app(id, deployed, "Succeeded", deployed, "Healthy")
		a.Ref, a.Sync = ref, sync
		h.tr.Observe(a)
	}
	obs("aaa1111", "Synced", 1, t0)

	// Desired tag changes first (image updater wrote it); the newest history entry is still old.
	obs("bbb2222", "OutOfSync", 1, t0)
	if len(h.deployed) != 0 {
		t.Fatal("desired-but-unsynced ref was treated as deployed")
	}
	// The sync completes: now it is live, with the new sync's timestamp.
	at := t0.Add(10 * time.Minute)
	obs("bbb2222", "Synced", 2, at)
	obs("bbb2222", "Synced", 2, at)
	if len(h.deployed) != 1 || h.deployed[0].PrevRef != "aaa1111" || !h.deployed[0].At.Equal(at) {
		t.Fatalf("deployed = %+v", h.deployed)
	}
}

func TestOutOfSyncBaselineHasNoLiveRef(t *testing.T) {
	h := newHarness(t)
	a := h.app(1, t0, "Succeeded", t0, "Healthy")
	a.Ref, a.Sync = "bbb2222", "OutOfSync" // started while a manual sync was pending
	h.tr.Observe(a)
	b := h.app(2, t0.Add(time.Hour), "Succeeded", t0.Add(time.Hour), "Healthy")
	b.Ref = "bbb2222"
	h.tr.Observe(b)
	if len(h.deployed) != 0 {
		t.Fatalf("emitted with an unknown previous ref: %+v", h.deployed)
	}
}
