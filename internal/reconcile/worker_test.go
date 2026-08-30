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
