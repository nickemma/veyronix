package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nickemma/plinth/internal/manifest"
)

type PostgresStore struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
}

func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	store := &PostgresStore{pool: pool}
	if err := store.ensureSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (s *PostgresStore) Close() { s.pool.Close() }

func (s *PostgresStore) ensureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS plinth_services (
                name text PRIMARY KEY,
                state jsonb NOT NULL,
                updated_at timestamptz NOT NULL
        );
		CREATE TABLE IF NOT EXISTS plinth_audit (
                id bigserial PRIMARY KEY,
                event_time timestamptz NOT NULL,
                actor text NOT NULL,
                team text NOT NULL,
                action text NOT NULL,
                resource text NOT NULL,
                revision integer NOT NULL,
                previous_revision integer NOT NULL,
                outcome text NOT NULL,
		        detail text NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS plinth_teams (
		        name text PRIMARY KEY,
		        members jsonb NOT NULL,
		        namespace text NOT NULL,
		        service_quota integer NOT NULL
		)`)
	return err
}

func (s *PostgresStore) Get(name string) (Service, error) {
	var data []byte
	err := s.pool.QueryRow(context.Background(), `SELECT state FROM plinth_services WHERE name=$1`, name).Scan(&data)
	if errors.Is(err, pgx.ErrNoRows) {
		return Service{}, os.ErrNotExist
	}
	if err != nil {
		return Service{}, err
	}
	var service Service
	if err := json.Unmarshal(data, &service); err != nil {
		return Service{}, fmt.Errorf("decode service state: %w", err)
	}
	return service, nil
}

func (s *PostgresStore) List() []Service {
	rows, err := s.pool.Query(context.Background(), `SELECT state FROM plinth_services ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	services := []Service{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			continue
		}
		var service Service
		if json.Unmarshal(data, &service) == nil {
			services = append(services, service)
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	return services
}

func (s *PostgresStore) Apply(m manifest.Manifest, reason string) (Service, error) {
	if err := m.Validate(); err != nil {
		return Service{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	service, err := s.Get(m.Name)
	if errors.Is(err, os.ErrNotExist) {
		service = Service{Name: m.Name, Phase: PhasePending}
	} else if err != nil {
		return Service{}, err
	} else if service.DesiredRevision > 0 {
		current, revisionErr := revision(service, service.DesiredRevision)
		if revisionErr == nil && reflect.DeepEqual(current.Manifest, m) && !service.Destroyed {
			return service, nil
		}
	}
	appendRevision(&service, m, reason)
	return service, s.save(service)
}

func (s *PostgresStore) Rollback(name string, target int) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, err := s.Get(name)
	if err != nil {
		return Service{}, err
	}
	if target == 0 {
		target = service.LastKnownGood
		if target == 0 {
			target = service.ActiveRevision
		}
	}
	selected, err := revision(service, target)
	if err != nil {
		return Service{}, err
	}
	appendRevision(&service, selected.Manifest, fmt.Sprintf("rollback to revision %d", target))
	return service, s.save(service)
}

func (s *PostgresStore) Update(name string, fn func(*Service) error) (Service, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, err := s.Get(name)
	if err != nil {
		return Service{}, err
	}
	if err := fn(&service); err != nil {
		return Service{}, err
	}
	service.UpdatedAt = time.Now().UTC()
	return service, s.save(service)
}

func (s *PostgresStore) Revision(name string, number int) (Revision, error) {
	service, err := s.Get(name)
	if err != nil {
		return Revision{}, err
	}
	return revision(service, number)
}

func (s *PostgresStore) AddAudit(record AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if record.Time.IsZero() {
		record.Time = time.Now().UTC()
	}
	_, err := s.pool.Exec(context.Background(), `INSERT INTO plinth_audit(event_time,actor,team,action,resource,revision,previous_revision,outcome,detail) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, record.Time, record.Actor, record.Team, record.Action, record.Resource, record.Revision, record.PreviousRevision, record.Outcome, record.Detail)
	return err
}

func (s *PostgresStore) SaveTeam(team TeamRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	members, err := json.Marshal(team.Members)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO plinth_teams(name,members,namespace,service_quota)
		VALUES($1,$2,$3,$4) ON CONFLICT(name) DO UPDATE SET members=EXCLUDED.members, namespace=EXCLUDED.namespace, service_quota=EXCLUDED.service_quota`, team.Name, members, team.Namespace, team.ServiceQuota)
	return err
}

func (s *PostgresStore) Teams() []TeamRecord {
	rows, err := s.pool.Query(context.Background(), `SELECT name,members,namespace,service_quota FROM plinth_teams ORDER BY name`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var result []TeamRecord
	for rows.Next() {
		var team TeamRecord
		var members []byte
		if rows.Scan(&team.Name, &members, &team.Namespace, &team.ServiceQuota) == nil && json.Unmarshal(members, &team.Members) == nil {
			result = append(result, team)
		}
	}
	return result
}

func (s *PostgresStore) Audit() []AuditRecord {
	rows, err := s.pool.Query(context.Background(), `SELECT id,event_time,actor,team,action,resource,revision,previous_revision,outcome,detail FROM plinth_audit ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var records []AuditRecord
	for rows.Next() {
		var record AuditRecord
		if rows.Scan(&record.ID, &record.Time, &record.Actor, &record.Team, &record.Action, &record.Resource, &record.Revision, &record.PreviousRevision, &record.Outcome, &record.Detail) == nil {
			records = append(records, record)
		}
	}
	return records
}

func (s *PostgresStore) save(service Service) error {
	data, err := json.Marshal(service)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(context.Background(), `INSERT INTO plinth_services(name,state,updated_at)
                VALUES($1,$2,$3)
                ON CONFLICT(name) DO UPDATE SET state=EXCLUDED.state, updated_at=EXCLUDED.updated_at`, service.Name, data, service.UpdatedAt)
	return err
}

func appendRevision(service *Service, m manifest.Manifest, reason string) {
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
}
