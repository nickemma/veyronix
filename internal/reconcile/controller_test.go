package reconcile

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/nickemma/plinth/internal/backend"
	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/state"
)

func testManifest(image string) manifest.Manifest {
	return manifest.Manifest{
		Name: "tessera-gateway", Image: image, Port: 8080, Replicas: 1,
		Env: map[string]string{"LOG_LEVEL": "info"}, Secrets: []string{"DATABASE_URL"},
		Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"},
	}
}

func newController(t *testing.T) (*Controller, *backend.Fake) {
	t.Helper()
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	fake := backend.NewFake()
	return NewController(store, fake), fake
}

func TestReconcileRepairsDrift(t *testing.T) {
	c, fake := newController(t)
	ctx := context.Background()
	service, err := c.Apply(ctx, testManifest("ghcr.io/example/tessera:v1"))
	if err != nil || service.Phase != state.PhaseReady {
		t.Fatalf("apply: service=%+v err=%v", service, err)
	}
	if err := fake.DeleteResource(ctx, service.Name, "Deployment"); err != nil {
		t.Fatal(err)
	}
	service, err = c.Reconcile(ctx, service.Name)
	if err != nil || len(service.Observed) != 9 {
		t.Fatalf("repair: service=%+v err=%v", service, err)
	}
}

func TestFailedRevisionAutomaticallyRollsBack(t *testing.T) {
	c, _ := newController(t)
	ctx := context.Background()
	first, err := c.Apply(ctx, testManifest("ghcr.io/example/tessera:v1"))
	if err != nil {
		t.Fatal(err)
	}
	failed, err := c.Apply(ctx, testManifest("ghcr.io/example/tessera:bad"))
	if err == nil || failed.Phase != state.PhaseRolledBack {
		t.Fatalf("expected rollback: service=%+v err=%v", failed, err)
	}
	if failed.ActiveRevision != first.ActiveRevision || failed.LastKnownGood != first.LastKnownGood {
		t.Fatalf("expected revision %d restored, got %+v", first.ActiveRevision, failed)
	}
	if failed.DesiredRevision != first.ActiveRevision {
		t.Fatalf("expected failed revision to be removed from desired state, got %d", failed.DesiredRevision)
	}
}

func TestProgressiveRolloutAbortsOnErrorRate(t *testing.T) {
	c, _ := newController(t)
	ctx := context.Background()
	if _, err := c.Apply(ctx, testManifest("ghcr.io/example/tessera:v1")); err != nil {
		t.Fatal(err)
	}
	m := testManifest("ghcr.io/example/tessera:error-rate")
	m.Rollout.Enabled = true
	m.Rollout.MaxErrorRate = 0.05
	service, err := c.Apply(ctx, m)
	if err == nil || service.Phase != state.PhaseRolledBack {
		t.Fatalf("expected rollout abort and rollback: service=%+v err=%v", service, err)
	}
}

func TestProgressiveRolloutAppliesEveryConfiguredStep(t *testing.T) {
	c, _ := newController(t)
	m := testManifest("ghcr.io/example/tessera:v1")
	m.Rollout.Enabled = true
	m.Rollout.Steps = []int{10, 50, 100}
	m.Rollout.MaxErrorRate = 0.05
	service, err := c.Apply(context.Background(), m)
	if err != nil || service.Phase != state.PhaseReady {
		t.Fatalf("expected successful progressive rollout: service=%+v err=%v", service, err)
	}
	for _, step := range []string{"10%", "50%", "100%"} {
		found := false
		for _, line := range service.Logs {
			if strings.Contains(line.Message, "rollout step "+step) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing rollout step %s in logs: %+v", step, service.Logs)
		}
	}
}

func TestCompletedProgressiveRolloutDoesNotRestartOnResync(t *testing.T) {
	c, _ := newController(t)
	m := testManifest("ghcr.io/example/tessera:v1")
	m.Rollout.Enabled = true
	m.Rollout.Steps = []int{10, 50, 100}
	m.Rollout.MaxErrorRate = 0.05
	service, err := c.Apply(context.Background(), m)
	if err != nil || service.Phase != state.PhaseReady || service.RolloutStep != 100 {
		t.Fatalf("expected completed rollout state, service=%+v err=%v", service, err)
	}
	countSteps := func() map[string]int {
		current, getErr := c.store.Get(m.Name)
		if getErr != nil {
			t.Fatal(getErr)
		}
		counts := map[string]int{}
		for _, line := range current.Logs {
			for _, step := range []string{"10%", "50%", "100%"} {
				if strings.Contains(line.Message, "rollout step "+step) {
					counts[step]++
				}
			}
		}
		return counts
	}
	before := countSteps()
	if _, err := c.Reconcile(context.Background(), m.Name); err != nil {
		t.Fatal(err)
	}
	after := countSteps()
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("resync restarted completed rollout: before=%v after=%v", before, after)
	}
}

func TestConcurrentReconcileKeepsOneRevision(t *testing.T) {
	c, _ := newController(t)
	ctx := context.Background()
	if _, err := c.Apply(ctx, testManifest("ghcr.io/example/tessera:v1")); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for range 2 {
		group.Add(1)
		go func() {
			defer group.Done()
			_, _ = c.Reconcile(ctx, "tessera-gateway")
		}()
	}
	group.Wait()
	service, err := c.store.Get("tessera-gateway")
	if err != nil {
		t.Fatal(err)
	}
	if len(service.History) != 1 || service.ActiveRevision != 1 {
		t.Fatalf("concurrent reconcile corrupted revisions: %+v", service)
	}
}
