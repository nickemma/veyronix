package state

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nickemma/plinth/internal/manifest"
)

func TestPostgresStoreIntegration(t *testing.T) {
	dsn := os.Getenv("PLINTH_TEST_DATABASE_DSN")
	if dsn == "" {
		t.Skip("set PLINTH_TEST_DATABASE_DSN to run the Postgres integration test")
	}
	store, err := OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	name := fmt.Sprintf("plinth-test-%d", time.Now().UnixNano())
	m := manifest.Manifest{Name: name, Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"}}
	if _, err := store.Apply(m, "integration"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(name, func(service *Service) error { service.Phase = PhaseReady; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTeam(TeamRecord{Name: "integration", Members: []string{"tester"}, Namespace: "plinth-integration", ServiceQuota: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddAudit(AuditRecord{Actor: "tester", Team: "integration", Action: "apply", Resource: name, Revision: 1}); err != nil {
		t.Fatal(err)
	}
	service, err := store.Get(name)
	if err != nil || service.Phase != PhaseReady {
		t.Fatalf("stored service: %+v err=%v", service, err)
	}
	if len(store.Teams()) == 0 || len(store.Audit()) == 0 {
		t.Fatal("expected persisted teams and audit records")
	}
	_, _ = store.pool.Exec(context.Background(), "DELETE FROM plinth_audit WHERE resource=$1", name)
	_, _ = store.pool.Exec(context.Background(), "DELETE FROM plinth_services WHERE name=$1", name)
}
