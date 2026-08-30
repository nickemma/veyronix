package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
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
