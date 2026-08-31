package reconcile

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/nickemma/plinth/internal/backend"
	"github.com/nickemma/plinth/internal/state"
)

func TestWorkerReconcilesQueuedDesiredState(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	controller := NewController(store, backend.NewFake())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := NewWorker(controller, time.Hour)
	worker.Start(ctx)
	service, err := controller.ApplyDesired(testManifest("ghcr.io/example/tessera:v1"))
	if err != nil {
		t.Fatal(err)
	}
	worker.Enqueue(service.Name)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		current, getErr := store.Get(service.Name)
		if getErr == nil && current.Phase == state.PhaseReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("queued service did not converge: %+v", service)
}

func TestWorkerRebuildsFromPersistedDesiredStateAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	firstStore, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstController := NewController(firstStore, backend.NewFake())
	if _, err := firstController.ApplyDesired(testManifest("ghcr.io/example/tessera:v1")); err != nil {
		t.Fatal(err)
	}

	restartedStore, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := NewWorker(NewController(restartedStore, backend.NewFake()), time.Hour)
	worker.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		service, getErr := restartedStore.Get("tessera-gateway")
		if getErr == nil && service.Phase == state.PhaseReady && service.ActiveRevision == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	service, _ := restartedStore.Get("tessera-gateway")
	t.Fatalf("restart did not converge persisted desired state: %+v", service)
}
