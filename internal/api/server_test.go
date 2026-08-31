package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nickemma/plinth/internal/backend"
	"github.com/nickemma/plinth/internal/reconcile"
	"github.com/nickemma/plinth/internal/state"
)

func TestEndToEndHTTPFlow(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(reconcile.NewController(store, backend.NewFake()), store)

	manifest := []byte(`{"name":"tessera-gateway","image":"ghcr.io/nickemma/tessera:v0.4.1","port":8080,"replicas":3,"env":{"LOG_LEVEL":"info"},"secrets":["DATABASE_URL"],"resources":{"cpu":"500m","memory":"512Mi"}}`)
	response := doRequest(t, server, "/api/v1/services", http.MethodPost, manifest)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("apply status: %s", response.Status)
	}
	var applied struct {
		Phase string `json:"phase"`
	}
	decodeResponse(t, response, &applied)
	if applied.Phase != state.PhaseReady {
		t.Fatalf("expected ready, got %q", applied.Phase)
	}
	audit := store.Audit()
	if len(audit) != 1 || audit[0].Revision != 1 || audit[0].PreviousRevision != 0 {
		t.Fatalf("unexpected initial audit revision data: %+v", audit)
	}

	response = doRequest(t, server, "/api/v1/services/tessera-gateway/test/drift", http.MethodPost, []byte(`{"kind":"Deployment"}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("drift status: %s", response.Status)
	}
	decodeResponse(t, response, &struct{}{})

	for _, path := range []string{"/docs", "/playground", "/openapi.yaml", "/api/v1/services/tessera-gateway/events", "/api/v1/services/tessera-gateway/logs"} {
		response = doRequest(t, server, path, http.MethodGet, nil)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status: %s", path, response.Status)
		}
		data, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if path == "/docs" && !strings.Contains(string(data), "SwaggerUIBundle") {
			t.Fatal("Swagger UI page did not load SwaggerUIBundle")
		}
		if path == "/playground" && (!strings.Contains(string(data), "Apply manifest") || !strings.Contains(string(data), "X-Plinth-Actor")) {
			t.Fatal("playground page did not expose the test controls and identity headers")
		}
		if path == "/openapi.yaml" && !strings.Contains(string(data), "/api/v1/services/{name}/rollback") {
			t.Fatal("OpenAPI document did not contain rollback operation")
		}
	}
	response = doRequest(t, server, "/api/v1/audit?format=csv", http.MethodGet, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("audit export status: %s", response.Status)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if !bytes.Contains(data, []byte("apply")) {
		t.Fatalf("audit export did not contain apply record: %s", data)
	}
}

func TestTeamPolicyAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(reconcile.NewController(store, backend.NewFake()), store)
	teamRequest := doRequest(t, server, "/api/v1/teams", http.MethodPost, []byte(`{"name":"payments","members":["alice"],"namespace":"plinth-payments","service_quota":20}`))
	if teamRequest.StatusCode != http.StatusOK {
		t.Fatalf("register team status: %d", teamRequest.StatusCode)
	}
	teamRequest.Body.Close()
	manifest := []byte(`{"name":"payments-api","image":"ghcr.io/example/payments:v1","port":8080,"replicas":1,"resources":{"cpu":"100m","memory":"128Mi"}}`)
	denied := httptest.NewRequest(http.MethodPost, "/api/v1/services", bytes.NewReader(manifest))
	denied.Header.Set("Content-Type", "application/json")
	denied.Header.Set("X-Plinth-Actor", "mallory")
	denied.Header.Set("X-Plinth-Team", "payments")
	deniedResponse := httptest.NewRecorder()
	server.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("expected team denial, got %d", deniedResponse.Code)
	}
	allowed := httptest.NewRequest(http.MethodPost, "/api/v1/services", bytes.NewReader(manifest))
	allowed.Header.Set("Content-Type", "application/json")
	allowed.Header.Set("X-Plinth-Actor", "alice")
	allowed.Header.Set("X-Plinth-Team", "payments")
	allowedResponse := httptest.NewRecorder()
	server.ServeHTTP(allowedResponse, allowed)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("expected team allow, got %d", allowedResponse.Code)
	}
	var allowedService state.Service
	if err := json.NewDecoder(allowedResponse.Body).Decode(&allowedService); err != nil {
		t.Fatal(err)
	}
	if allowedService.Team != "payments" || allowedService.History[0].Manifest.Namespace != "plinth-payments" {
		t.Fatalf("expected team namespace assignment, got %+v", allowedService)
	}
	list := httptest.NewRequest(http.MethodGet, "/api/v1/services", nil)
	list.Header.Set("X-Plinth-Actor", "alice")
	list.Header.Set("X-Plinth-Team", "payments")
	listResponse := httptest.NewRecorder()
	server.ServeHTTP(listResponse, list)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("expected team list allow, got %d", listResponse.Code)
	}
	var services []state.Service
	if err := json.NewDecoder(listResponse.Body).Decode(&services); err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].Name != "payments-api" {
		t.Fatalf("expected only payments service, got %+v", services)
	}

	other := httptest.NewRequest(http.MethodGet, "/api/v1/services/payments-api", nil)
	other.Header.Set("X-Plinth-Actor", "mallory")
	other.Header.Set("X-Plinth-Team", "payments")
	otherResponse := httptest.NewRecorder()
	server.ServeHTTP(otherResponse, other)
	if otherResponse.Code != http.StatusForbidden {
		t.Fatalf("expected service read denial, got %d", otherResponse.Code)
	}
	reloaded, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewServer(reconcile.NewController(reloaded, backend.NewFake()), reloaded)
	listAfterRestart := httptest.NewRequest(http.MethodGet, "/api/v1/teams", nil)
	listAfterRestart.Header.Set("X-Plinth-Actor", "local")
	restartedResponse := httptest.NewRecorder()
	restarted.ServeHTTP(restartedResponse, listAfterRestart)
	if restartedResponse.Code != http.StatusOK {
		t.Fatalf("expected persisted team list, got %d", restartedResponse.Code)
	}
	var teams []map[string]any
	if err := json.NewDecoder(restartedResponse.Body).Decode(&teams); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, team := range teams {
		if team["name"] == "payments" {
			found = true
		}
	}
	if !found {
		t.Fatalf("persisted team not found: %+v", teams)
	}
}

func TestTeamCannotReassignAnotherTeamsService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := state.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(reconcile.NewController(store, backend.NewFake()), store)
	teamRequest := doRequest(t, server, "/api/v1/teams", http.MethodPost, []byte(`{"name":"payments","members":["alice"],"namespace":"plinth-payments","service_quota":20}`))
	teamRequest.Body.Close()
	manifest := []byte(`{"name":"shared-api","image":"ghcr.io/example/shared:v1","port":8080,"replicas":1,"resources":{"cpu":"100m","memory":"128Mi"}}`)
	response := doRequest(t, server, "/api/v1/services", http.MethodPost, manifest)
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		t.Fatalf("expected default service creation, got %d", response.StatusCode)
	}
	response.Body.Close()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/services", bytes.NewReader(manifest))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Plinth-Actor", "alice")
	request.Header.Set("X-Plinth-Team", "payments")
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("expected cross-team reassignment denial, got %d", recorder.Code)
	}
}

func TestConfiguredDefaultNamespaceIsAppliedToServices(t *testing.T) {
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	server := NewServerWithWorkerAndNamespace(reconcile.NewController(store, backend.NewFake()), store, nil, "plinth-test")
	response := doRequest(t, server, "/api/v1/services", http.MethodPost, []byte(`{"name":"namespace-check","image":"ghcr.io/example/check:v1","port":8080,"replicas":1,"resources":{"cpu":"100m","memory":"128Mi"}}`))
	if response.StatusCode != http.StatusOK {
		t.Fatalf("apply status: %d", response.StatusCode)
	}
	var service state.Service
	decodeResponse(t, response, &service)
	if service.History[0].Manifest.Namespace != "plinth-test" {
		t.Fatalf("expected configured default namespace, got %+v", service.History[0].Manifest)
	}
}

func doRequest(t *testing.T, handler http.Handler, path, method string, body []byte) *http.Response {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Result()
}

func decodeResponse(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
