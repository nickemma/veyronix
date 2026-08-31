package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/nickemma/plinth/internal/manifest"
	"github.com/nickemma/plinth/internal/reconcile"
	"github.com/nickemma/plinth/internal/state"
	"github.com/nickemma/plinth/internal/tenancy"
)

type Server struct {
	controller *reconcile.Controller
	store      state.Repository
	worker     *reconcile.Worker
	policy     *tenancy.Policy
}

func NewServer(controller *reconcile.Controller, store state.Repository) *Server {
	return newServer(controller, store, nil, "plinth-default")
}

func NewServerWithWorker(controller *reconcile.Controller, store state.Repository, worker *reconcile.Worker) *Server {
	return newServer(controller, store, worker, "plinth-default")
}

func NewServerWithWorkerAndNamespace(controller *reconcile.Controller, store state.Repository, worker *reconcile.Worker, namespace string) *Server {
	if namespace == "" {
		namespace = "plinth-default"
	}
	return newServer(controller, store, worker, namespace)
}

func newServer(controller *reconcile.Controller, store state.Repository, worker *reconcile.Worker, defaultNamespace string) *Server {
	return &Server{controller: controller, store: store, worker: worker, policy: loadPolicy(store, defaultNamespace)}
}

func loadPolicy(store state.Repository, defaultNamespace string) *tenancy.Policy {
	policy := tenancy.NewPolicy()
	if defaultNamespace != "" && defaultNamespace != "plinth-default" {
		_ = policy.Register(tenancy.Team{Name: "default", Members: []string{"local", "*"}, Namespace: defaultNamespace, ServiceQuota: 100})
	}
	if teams, ok := store.(state.TeamRepository); ok {
		for _, record := range teams.Teams() {
			_ = policy.Register(tenancy.Team{Name: record.Name, Members: record.Members, Namespace: record.Namespace, ServiceQuota: record.ServiceQuota})
		}
	}
	return policy
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
		spec := openAPISpec()
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write(spec)
		return
	}
	if r.URL.Path == "/api/v1/audit" {
		s.handleAudit(w, r)
		return
	}
	if r.URL.Path == "/api/v1/teams" {
		s.handleTeams(w, r)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/v1/services") {
		http.NotFound(w, r)
		return
	}
	s.handleServices(w, r)
}

func openAPISpec() []byte {
	candidates := []string{filepath.Join("api", "openapi.yaml"), filepath.Join("..", "..", "api", "openapi.yaml")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "api", "openapi.yaml"))
	}
	for _, path := range candidates {
		if spec, err := os.ReadFile(path); err == nil {
			return spec
		}
	}
	return []byte(openAPIFallback)
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) == 3 && parts[2] == "services" {
		if r.Method == http.MethodGet {
			actor, team := identity(r)
			if err := s.authorize(actor, team, "list", "services"); err != nil {
				writeError(w, http.StatusForbidden, err)
				return
			}
			services := s.store.List()
			if !isGlobalActor(actor) {
				services = filterServicesByTeam(services, team)
			}
			writeJSON(w, http.StatusOK, services)
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
			actor, team := identity(r)
			if err := s.authorize(actor, team, "apply", m.Name); err != nil {
				s.recordAudit(actor, team, "apply", m.Name, state.Service{}, 0, err)
				writeError(w, http.StatusForbidden, err)
				return
			}
			if !s.policy.WithinQuota(team, teamServiceNames(s.store.List(), team), m.Name) {
				err := fmt.Errorf("team %q service quota exceeded", team)
				s.recordAudit(actor, team, "apply", m.Name, state.Service{}, 0, err)
				writeError(w, http.StatusTooManyRequests, err)
				return
			}
			if existing, existingErr := s.store.Get(m.Name); existingErr == nil && existing.Team != "" && existing.Team != team && actor != "local" && actor != "admin" {
				err := fmt.Errorf("service %q belongs to team %q", m.Name, existing.Team)
				s.recordAudit(actor, team, "apply", m.Name, existing, existing.ActiveRevision, err)
				writeError(w, http.StatusForbidden, err)
				return
			}
			configured, _ := s.policy.Team(team)
			m.Namespace = configured.Namespace
			previous, previousErr := s.store.Get(m.Name)
			previousRevision := 0
			requestedRevision := 1
			if previousErr == nil {
				previousRevision = previous.ActiveRevision
				requestedRevision = len(previous.History) + 1
			}
			var service state.Service
			var err error
			if s.worker != nil {
				service, err = s.controller.ApplyDesired(m)
				if err == nil {
					service, err = s.store.Update(m.Name, func(current *state.Service) error { current.Team = team; return nil })
					if err == nil {
						s.worker.Enqueue(m.Name)
					}
				}
			} else {
				service, err = s.controller.Apply(r.Context(), m)
				if err == nil {
					service, err = s.store.Update(m.Name, func(current *state.Service) error { current.Team = team; return nil })
				}
			}
			if err != nil && service.Name == "" {
				s.recordAuditRevision(actor, team, "apply", m.Name, service, requestedRevision, previousRevision, err)
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			auditRevision := requestedRevision
			if len(service.History) > 0 {
				auditRevision = service.History[len(service.History)-1].Number
			}
			s.recordAuditRevision(actor, team, "apply", m.Name, service, auditRevision, previousRevision, err)
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
		actor, team := identity(r)
		if err := s.authorizeService(actor, team, "get", service); err != nil {
			writeError(w, http.StatusForbidden, err)
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
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		actor, team := identity(r)
		if err := s.authorizeService(actor, team, action, service); err != nil {
			writeError(w, http.StatusForbidden, err)
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
	actor, team := identity(r)
	current, currentErr := s.store.Get(name)
	if currentErr != nil {
		status := http.StatusInternalServerError
		if errors.Is(currentErr, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, currentErr)
		return
	}
	if err := s.authorizeService(actor, team, action, current); err != nil {
		s.recordAudit(actor, team, action, name, state.Service{}, current.ActiveRevision, err)
		writeError(w, http.StatusForbidden, err)
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
		s.recordAudit(actor, team, action, name, service, current.ActiveRevision, err)
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	s.recordAudit(actor, team, action, name, service, current.ActiveRevision, err)
	writeJSON(w, http.StatusOK, map[string]any{"service": service, "error": errorText(err)})
}

func (s *Server) authorize(actor, team, action, name string) error {
	if s.policy == nil {
		s.policy = tenancy.NewPolicy()
	}
	return s.policy.Authorize(actor, team, action)
}

func identity(r *http.Request) (string, string) {
	actor := r.Header.Get("X-Plinth-Actor")
	if actor == "" {
		actor = "local"
	}
	team := r.Header.Get("X-Plinth-Team")
	if team == "" {
		team = "default"
	}
	return actor, team
}

func teamServiceNames(services []state.Service, team string) []string {
	result := []string{}
	for _, service := range services {
		if service.Team == team || (service.Team == "" && team == "default") {
			result = append(result, service.Name)
		}
	}
	return result
}

func filterServicesByTeam(services []state.Service, team string) []state.Service {
	result := make([]state.Service, 0, len(services))
	for _, service := range services {
		if service.Team == team || (service.Team == "" && team == "default") {
			result = append(result, service)
		}
	}
	return result
}

func (s *Server) authorizeService(actor, team, action string, service state.Service) error {
	if err := s.authorize(actor, team, action, service.Name); err != nil {
		return err
	}
	if !isGlobalActor(actor) && service.Team != "" && service.Team != team {
		return fmt.Errorf("service %q belongs to team %q", service.Name, service.Team)
	}
	return nil
}

func isGlobalActor(actor string) bool { return actor == "local" || actor == "admin" }

func (s *Server) recordAudit(actor, team, action, resource string, service state.Service, previousRevision int, err error) {
	s.recordAuditRevision(actor, team, action, resource, service, service.DesiredRevision, previousRevision, err)
}

func (s *Server) recordAuditRevision(actor, team, action, resource string, service state.Service, revision, previousRevision int, err error) {
	auditor, ok := s.store.(state.Auditor)
	if !ok {
		return
	}
	outcome := "accepted"
	if err != nil {
		outcome = "denied_or_failed"
	}
	_ = auditor.AddAudit(state.AuditRecord{Actor: actor, Team: team, Action: action, Resource: resource, Revision: revision, PreviousRevision: previousRevision, Outcome: outcome, Detail: errorText(err)})
}

func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		actor, team := identity(r)
		if actor != "local" && actor != "admin" {
			if err := s.authorize(actor, team, "list", "teams"); err != nil {
				writeError(w, http.StatusForbidden, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, s.policy.List())
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	actor, _ := identity(r)
	if actor != "local" && actor != "admin" {
		writeError(w, http.StatusForbidden, fmt.Errorf("only local or admin actors can register teams"))
		return
	}
	var team tenancy.Team
	if err := decodeJSON(r, &team); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.policy.Register(team); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.controller.EnsureNamespace(r.Context(), team.Namespace); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("ensure team namespace: %w", err))
		return
	}
	if teams, ok := s.store.(state.TeamRepository); ok {
		if err := teams.SaveTeam(state.TeamRecord{Name: team.Name, Members: team.Members, Namespace: team.Namespace, ServiceQuota: team.ServiceQuota}); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, team)
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	auditor, ok := s.store.(state.Auditor)
	if !ok {
		writeJSON(w, http.StatusOK, []state.AuditRecord{})
		return
	}
	records := auditor.Audit()
	from, to := time.Time{}, time.Time{}
	var err error
	if value := r.URL.Query().Get("from"); value != "" {
		from, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid from timestamp: %w", err))
			return
		}
	}
	if value := r.URL.Query().Get("to"); value != "" {
		to, err = time.Parse(time.RFC3339, value)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid to timestamp: %w", err))
			return
		}
	}
	filtered := records[:0]
	for _, record := range records {
		if (!from.IsZero() && record.Time.Before(from)) || (!to.IsZero() && record.Time.After(to)) {
			continue
		}
		filtered = append(filtered, record)
	}
	if r.URL.Query().Get("format") == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		writer := csv.NewWriter(w)
		_ = writer.Write([]string{"id", "time", "actor", "team", "action", "resource", "revision", "previous_revision", "outcome", "detail"})
		for _, record := range filtered {
			_ = writer.Write([]string{strconv.Itoa(record.ID), record.Time.Format(time.RFC3339), record.Actor, record.Team, record.Action, record.Resource, strconv.Itoa(record.Revision), strconv.Itoa(record.PreviousRevision), record.Outcome, record.Detail})
		}
		writer.Flush()
		return
	}
	writeJSON(w, http.StatusOK, filtered)
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
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Plinth-Actor, X-Plinth-Team")
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
