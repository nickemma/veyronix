package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/reconcile"
	"github.com/nickemma/plinth/internal/state"
)

type Server struct {
	controller *reconcile.Controller
	store      *state.Store
	worker     *reconcile.Worker
}

func NewServer(controller *reconcile.Controller, store *state.Store) *Server {
	return &Server{controller: controller, store: store}
}

func NewServerWithWorker(controller *reconcile.Controller, store *state.Store, worker *reconcile.Worker) *Server {
	return &Server{controller: controller, store: store, worker: worker}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		writeCORS(w)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeCORS(w)
	if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/docs" || r.URL.Path == "/docs/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(swaggerHTML))
		return
	}
	if r.URL.Path == "/playground" || r.URL.Path == "/playground/" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(playgroundHTML))
		return
	}
	if r.URL.Path == "/openapi.yaml" {
		spec, err := os.ReadFile(filepath.Join("api", "openapi.yaml"))
		if err != nil {
			spec = []byte(openAPIFallback)
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(spec)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/services") {
		http.NotFound(w, r)
		return
	}
	s.handleServices(w, r)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 && parts[2] == "services" {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, s.store.List())
			return
		}
		if r.Method == http.MethodPost {
			var m manifest.Manifest
			if err := decodeJSON(r, &m); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if err := m.Validate(); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			var service state.Service
			var err error
			if s.worker != nil {
				service, err = s.controller.ApplyDesired(m)
				s.worker.Enqueue(m.Name)
			} else {
				service, err = s.controller.Apply(r.Context(), m)
			}
			if err != nil && service.Name == "" {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, http.StatusOK, service)
			return
		}
	}
	if len(parts) < 4 || parts[2] != "services" || parts[3] == "" {
		http.NotFound(w, r)
		return
	}
	name := parts[3]
	if len(parts) == 4 && r.Method == http.MethodGet {
		service, err := s.store.Get(name)
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Errorf("service %q not found", name))
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, service)
		return
	}
	if len(parts) < 5 {
		http.NotFound(w, r)
		return
	}
	action := strings.Join(parts[4:], "/")
	if action == "events" || action == "logs" {
		service, err := s.store.Get(name)
		if errors.Is(err, os.ErrNotExist) {
			writeError(w, http.StatusNotFound, fmt.Errorf("service %q not found", name))
			return
		}
		if action == "events" {
			writeJSON(w, http.StatusOK, service.Events)
		} else {
			writeJSON(w, http.StatusOK, service.Logs)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var service state.Service
	var err error
	switch action {
	case "rollback":
		var request struct {
			Revision int `json:"revision"`
		}
		if r.ContentLength > 0 {
			if err = decodeJSON(r, &request); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
		}
		service, err = s.controller.Rollback(r.Context(), name, request.Revision)
	case "pause":
		service, err = s.controller.Pause(name)
	case "resume":
		service, err = s.controller.Resume(r.Context(), name)
	case "destroy":
		service, err = s.controller.Destroy(r.Context(), name)
	case "test/drift":
		var request struct {
			Kind string `json:"kind"`
		}
		if err = decodeJSON(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if request.Kind == "" {
			request.Kind = "Deployment"
		}
		service, err = s.controller.Drift(r.Context(), name, request.Kind)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil && service.Name == "" {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"service": service, "error": errorText(err)})
}

func decodeJSON(r *http.Request, target any) error {
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

func writeCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

var _ http.Handler = (*Server)(nil)
