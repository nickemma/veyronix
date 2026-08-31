package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/nickemma/plinth/internal/manifest"
)

const (
	PhasePending     = "pending"
	PhaseReconciling = "reconciling"
	PhaseReady       = "ready"
	PhaseRolledBack  = "rolled_back"
	PhaseFailed      = "failed"
	PhasePaused      = "paused"
	PhaseDestroyed   = "destroyed"
)

type Event struct {
	Time    time.Time `json:"time"`
	Type    string    `json:"type"`
	Message string    `json:"message"`
}

type LogLine struct {
	Time    time.Time `json:"time"`
	Stream  string    `json:"stream"`
	Message string    `json:"message"`
}

type AuditRecord struct {
	ID               int       `json:"id"`
	Time             time.Time `json:"time"`
	Actor            string    `json:"actor"`
	Team             string    `json:"team"`
	Action           string    `json:"action"`
	Resource         string    `json:"resource"`
	Revision         int       `json:"revision"`
	PreviousRevision int       `json:"previous_revision"`
	Outcome          string    `json:"outcome"`
	Detail           string    `json:"detail,omitempty"`
}

// TeamRecord is the persistence-neutral representation of tenancy policy.
// The API layer maps it to its policy type so state storage remains usable by
// both the file and Postgres implementations.
type TeamRecord struct {
	Name         string   `json:"name"`
	Members      []string `json:"members"`
	Namespace    string   `json:"namespace"`
	ServiceQuota int      `json:"service_quota"`
}

type Revision struct {
	Number    int               `json:"number"`
	Manifest  manifest.Manifest `json:"manifest"`
	CreatedAt time.Time         `json:"created_at"`
	Reason    string            `json:"reason"`
}

type Service struct {
	Name            string     `json:"name"`
	Team            string     `json:"team"`
	DesiredRevision int        `json:"desired_revision"`
	ActiveRevision  int        `json:"active_revision"`
	LastKnownGood   int        `json:"last_known_good"`
	RolloutStep     int        `json:"rollout_step,omitempty"`
	Paused          bool       `json:"paused"`
	Destroyed       bool       `json:"destroyed"`
	Phase           string     `json:"phase"`
	Message         string     `json:"message"`
	Observed        []string   `json:"observed,omitempty"`
	Events          []Event    `json:"events,omitempty"`
	Logs            []LogLine  `json:"logs,omitempty"`
	History         []Revision `json:"history"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type database struct {
	Services map[string]*Service   `json:"services"`
	Audit    []AuditRecord         `json:"audit"`
	Teams    map[string]TeamRecord `json:"teams,omitempty"`
}

type Store struct {
	mu   sync.Mutex
	path string
	db   database
}

// Repository is the persistence seam. The file-backed implementation keeps
// local development dependency-free; Postgres implements the same contract
// for the control-plane deployment.
type Repository interface {
	Get(string) (Service, error)
	List() []Service
	Apply(manifest.Manifest, string) (Service, error)
	Rollback(string, int) (Service, error)
	Update(string, func(*Service) error) (Service, error)
	Revision(string, int) (Revision, error)
}

type Auditor interface {
	AddAudit(AuditRecord) error
	Audit() []AuditRecord
}

type TeamRepository interface {
	SaveTeam(TeamRecord) error
	Teams() []TeamRecord
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, db: database{Services: map[string]*Service{}, Teams: map[string]TeamRecord{}}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.db); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if s.db.Services == nil {
		s.db.Services = map[string]*Service{}
	}
	if s.db.Teams == nil {
		s.db.Teams = map[string]TeamRecord{}
	}
	return s, nil
}

func (s *Store) SaveTeam(team TeamRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db.Teams == nil {
		s.db.Teams = map[string]TeamRecord{}
	}
	team.Members = append([]string(nil), team.Members...)
	s.db.Teams[team.Name] = team
	return s.persistLocked()
}

func (s *Store) Teams() []TeamRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]TeamRecord, 0, len(s.db.Teams))
	for _, team := range s.db.Teams {
		team.Members = append([]string(nil), team.Members...)
		result = append(result, team)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (s *Store) AddAudit(record AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	record.ID = len(s.db.Audit) + 1
	if record.Time.IsZero() {
		record.Time = time.Now().UTC()
	}
	s.db.Audit = append(s.db.Audit, record)
	return s.persistLocked()
}

func (s *Store) Audit() []AuditRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := append([]AuditRecord(nil), s.db.Audit...)
	return result
}

func (s *Store) Get(name string) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.db.Services[name]
	if !ok {
		return Service{}, os.ErrNotExist
	}
	return clone(*service), nil
}

func (s *Store) List() []Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Service, 0, len(s.db.Services))
	for _, service := range s.db.Services {
		out = append(out, clone(*service))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (s *Store) Apply(m manifest.Manifest, reason string) (Service, error) {
	if err := m.Validate(); err != nil {
		return Service{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	service := s.db.Services[m.Name]
	if service == nil {
		service = &Service{Name: m.Name, Phase: PhasePending}
		s.db.Services[m.Name] = service
	}
	if !service.Destroyed && service.DesiredRevision > 0 {
		current, err := revision(*service, service.DesiredRevision)
		if err == nil && reflect.DeepEqual(current.Manifest, m) {
			return clone(*service), nil
		}
	}
	number := len(service.History) + 1
	service.History = append(service.History, Revision{Number: number, Manifest: m, CreatedAt: time.Now().UTC(), Reason: reason})
	service.DesiredRevision = number
	service.RolloutStep = 0
	service.Paused = false
	service.Destroyed = false
	service.Phase = PhasePending
	service.Message = fmt.Sprintf("revision %d is waiting for reconciliation", number)
	service.UpdatedAt = time.Now().UTC()
	addEvent(service, "revision_created", service.Message)
	if err := s.persistLocked(); err != nil {
		return Service{}, err
	}
	return clone(*service), nil
}

func (s *Store) Rollback(name string, target int) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.db.Services[name]
	if !ok {
		return Service{}, os.ErrNotExist
	}
	if target == 0 {
		target = service.LastKnownGood
		if target == 0 {
			target = service.ActiveRevision
		}
	}
	selected, err := revision(*service, target)
	if err != nil {
		return Service{}, err
	}
	number := len(service.History) + 1
	service.History = append(service.History, Revision{Number: number, Manifest: selected.Manifest, CreatedAt: time.Now().UTC(), Reason: fmt.Sprintf("rollback to revision %d", target)})
	service.DesiredRevision = number
	service.RolloutStep = 0
	service.Paused = false
	service.Destroyed = false
	service.Phase = PhasePending
	service.Message = fmt.Sprintf("rollback revision %d is waiting for reconciliation", number)
	service.UpdatedAt = time.Now().UTC()
	addEvent(service, "rollback_requested", service.Message)
	if err := s.persistLocked(); err != nil {
		return Service{}, err
	}
	return clone(*service), nil
}

func (s *Store) Update(name string, fn func(*Service) error) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.db.Services[name]
	if !ok {
		return Service{}, os.ErrNotExist
	}
	if err := fn(service); err != nil {
		return Service{}, err
	}
	service.UpdatedAt = time.Now().UTC()
	if err := s.persistLocked(); err != nil {
		return Service{}, err
	}
	return clone(*service), nil
}

func (s *Store) Revision(name string, number int) (Revision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.db.Services[name]
	if !ok {
		return Revision{}, os.ErrNotExist
	}
	return revision(*service, number)
}

func (s *Store) persistLocked() error {
	if s.path == "" {
		return nil
	}
	if dir := filepath.Dir(s.path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".plinth-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	data, err := json.MarshalIndent(s.db, "", "  ")
	if err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}

func revision(service Service, number int) (Revision, error) {
	for _, item := range service.History {
		if item.Number == number {
			return item, nil
		}
	}
	return Revision{}, fmt.Errorf("revision %d does not exist", number)
}

func addEvent(service *Service, eventType, message string) {
	service.Events = append(service.Events, Event{Time: time.Now().UTC(), Type: eventType, Message: message})
	if len(service.Events) > 200 {
		service.Events = service.Events[len(service.Events)-200:]
	}
}

func AddEvent(service *Service, eventType, message string) { addEvent(service, eventType, message) }

func AddLog(service *Service, stream, message string) {
	service.Logs = append(service.Logs, LogLine{Time: time.Now().UTC(), Stream: stream, Message: message})
	if len(service.Logs) > 500 {
		service.Logs = service.Logs[len(service.Logs)-500:]
	}
}

func clone(service Service) Service {
	data, _ := json.Marshal(service)
	var copied Service
	_ = json.Unmarshal(data, &copied)
	return copied
}
