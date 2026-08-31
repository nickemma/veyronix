package state

import (
	"path/filepath"
	"testing"

	"github.com/nickemma/plinth/internal/manifest"
)

func TestStorePersistsRevisionsAuditAndTeams(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	serviceManifest := manifest.Manifest{Name: "tessera", Image: "ghcr.io/example/tessera:v1", Port: 8080, Replicas: 2, Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"}}
	if _, err := store.Apply(serviceManifest, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveTeam(TeamRecord{Name: "payments", Members: []string{"alice"}, Namespace: "plinth-payments", ServiceQuota: 10}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddAudit(AuditRecord{Actor: "alice", Team: "payments", Action: "apply", Resource: "tessera", Revision: 1}); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	service, err := reloaded.Get("tessera")
	if err != nil || len(service.History) != 1 {
		t.Fatalf("reloaded service: %+v, err=%v", service, err)
	}
	teams := reloaded.Teams()
	if len(teams) != 1 || teams[0].Namespace != "plinth-payments" {
		t.Fatalf("reloaded teams: %+v", teams)
	}
	if audit := reloaded.Audit(); len(audit) != 1 || audit[0].Actor != "alice" {
		t.Fatalf("reloaded audit: %+v", audit)
	}
}

func TestStoreDoesNotCreateDuplicateRevisionForSameManifest(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	m := manifest.Manifest{Name: "tessera", Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"}}
	first, err := store.Apply(m, "apply")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Apply(m, "apply")
	if err != nil {
		t.Fatal(err)
	}
	if first.DesiredRevision != second.DesiredRevision || len(second.History) != 1 {
		t.Fatalf("duplicate revision: first=%+v second=%+v", first, second)
	}
}

func TestStoreRejectsInvalidManifestBeforePersisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	invalid := manifest.Manifest{Name: "not valid", Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"}}
	if _, err := store.Apply(invalid, "apply"); err == nil {
		t.Fatal("expected invalid manifest to be rejected by the state boundary")
	}
	if services := store.List(); len(services) != 0 {
		t.Fatalf("invalid manifest was persisted: %+v", services)
	}
}

func TestStoreRollbackCreatesDesiredRevisionFromAnyHistoricalRevision(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	firstManifest := manifest.Manifest{Name: "tessera", Image: "example/tessera:v1", Port: 8080, Replicas: 1, Resources: manifest.Resources{CPU: "100m", Memory: "128Mi"}}
	secondManifest := firstManifest
	secondManifest.Image = "example/tessera:v2"
	thirdManifest := firstManifest
	thirdManifest.Image = "example/tessera:v3"
	if _, err := store.Apply(firstManifest, "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(secondManifest, "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Apply(thirdManifest, "apply"); err != nil {
		t.Fatal(err)
	}
	service, err := store.Rollback("tessera", 1)
	if err != nil {
		t.Fatal(err)
	}
	if service.DesiredRevision != 4 || service.History[3].Manifest.Image != firstManifest.Image {
		t.Fatalf("expected revision four to restore revision one, got %+v", service)
	}
}
