package reconcile

import (
	"context"
	"path/filepath"
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
}
