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

type Revision struct {
	Number    int               `json:"number"`
	Manifest  manifest.Manifest `json:"manifest"`
	CreatedAt time.Time         `json:"created_at"`
	Reason    string            `json:"reason"`
}

type Service struct {
	Name            string     `json:"name"`
	DesiredRevision int        `json:"desired_revision"`
	ActiveRevision  int        `json:"active_revision"`
	LastKnownGood   int        `json:"last_known_good"`
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
	Services map[string]*Service `json:"services"`
}

type Store struct {
	mu   sync.Mutex
	path string
	db   database
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, db: database{Services: map[string]*Service{}}}
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
	return s, nil
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
