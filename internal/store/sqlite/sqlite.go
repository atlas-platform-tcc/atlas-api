// Package sqlite implements the catalog Store on top of SQLite (pure-Go driver,
// no CGO). The catalog is a projection/audit record, not the source of truth.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS services (
	name             TEXT PRIMARY KEY,
	language         TEXT NOT NULL,
	replicas         INTEGER NOT NULL,
	database_enabled INTEGER NOT NULL,
	database_type    TEXT NOT NULL DEFAULT '',
	status           TEXT NOT NULL,
	repo_url         TEXT NOT NULL DEFAULT '',
	infra_pr_url     TEXT NOT NULL DEFAULT '',
	gitops_pr_url    TEXT NOT NULL DEFAULT '',
	url              TEXT NOT NULL DEFAULT '',
	env_json         TEXT NOT NULL DEFAULT '[]',
	conformance_json TEXT NOT NULL DEFAULT '',
	provision_ms     INTEGER NOT NULL DEFAULT 0,
	created_at       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	id       INTEGER PRIMARY KEY AUTOINCREMENT,
	service  TEXT NOT NULL,
	type     TEXT NOT NULL,
	outcome  TEXT NOT NULL,
	actor    TEXT NOT NULL DEFAULT '',
	detail   TEXT NOT NULL DEFAULT '',
	at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_service ON events(service);`

// Store is a SQLite-backed catalog.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the SQLite database at dsn and applies the schema.
// Use dsn ":memory:" for an ephemeral database.
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies idempotent column additions so databases created by earlier
// versions gain new columns without being recreated.
func migrate(db *sql.DB) error {
	stmts := []string{
		`ALTER TABLE services ADD COLUMN repo_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE services ADD COLUMN infra_pr_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE services ADD COLUMN gitops_pr_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE services ADD COLUMN env_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE services ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE services ADD COLUMN conformance_json TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE services ADD COLUMN provision_ms INTEGER NOT NULL DEFAULT 0`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(context.Background(), stmt); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// Close releases the underlying database handle.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Create(ctx context.Context, svc domain.Service) error {
	if _, err := s.Get(ctx, svc.Name); err == nil {
		return store.ErrAlreadyExists
	} else if err != store.ErrNotFound {
		return err
	}
	createdAt := svc.CreatedAt
	if createdAt == "" {
		createdAt = time.Now().UTC().Format(time.RFC3339)
	}
	envJSON, err := marshalEnv(svc.Env)
	if err != nil {
		return err
	}
	confJSON, err := marshalConformance(svc.Conformance)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO services (name, language, replicas, database_enabled, database_type, status, repo_url, infra_pr_url, gitops_pr_url, url, env_json, conformance_json, provision_ms, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		svc.Name, svc.Language, svc.Replicas, boolToInt(svc.Database.Enabled), svc.Database.Type, svc.Status, svc.RepoURL, svc.InfraPRURL, svc.GitopsPRURL, svc.URL, envJSON, confJSON, svc.ProvisionMs, createdAt)
	if err != nil {
		return fmt.Errorf("insert service: %w", err)
	}
	return nil
}

func (s *Store) Update(ctx context.Context, svc domain.Service) error {
	envJSON, err := marshalEnv(svc.Env)
	if err != nil {
		return err
	}
	confJSON, err := marshalConformance(svc.Conformance)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE services SET language=?, replicas=?, database_enabled=?, database_type=?, status=?, repo_url=?, infra_pr_url=?, gitops_pr_url=?, url=?, env_json=?, conformance_json=?, provision_ms=?
		 WHERE name=?`,
		svc.Language, svc.Replicas, boolToInt(svc.Database.Enabled), svc.Database.Type, svc.Status, svc.RepoURL, svc.InfraPRURL, svc.GitopsPRURL, svc.URL, envJSON, confJSON, svc.ProvisionMs, svc.Name)
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update service: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

// Delete removes a service from the catalog. Only the projected row is dropped: the
// audit trail lives in Git (teardown Pull Requests and the archived repository,
// ADR atlas-api/0011).
func (s *Store) Delete(ctx context.Context, name string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete service: %w", err)
	}
	if n == 0 {
		return store.ErrNotFound
	}
	return nil
}

func (s *Store) Get(ctx context.Context, name string) (domain.Service, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, language, replicas, database_enabled, database_type, status, repo_url, infra_pr_url, gitops_pr_url, url, env_json, conformance_json, provision_ms, created_at
		 FROM services WHERE name = ?`, name)
	svc, err := scan(row)
	if err == sql.ErrNoRows {
		return domain.Service{}, store.ErrNotFound
	}
	if err != nil {
		return domain.Service{}, fmt.Errorf("get service: %w", err)
	}
	return svc, nil
}

func (s *Store) List(ctx context.Context) ([]domain.Service, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, language, replicas, database_enabled, database_type, status, repo_url, infra_pr_url, gitops_pr_url, url, env_json, conformance_json, provision_ms, created_at
		 FROM services ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	defer rows.Close()

	var out []domain.Service
	for rows.Next() {
		svc, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("scan service: %w", err)
		}
		out = append(out, svc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate services: %w", err)
	}
	return out, nil
}

// AppendEvent inserts an audit event (append-only; ADR atlas-api/0017).
func (s *Store) AppendEvent(ctx context.Context, e domain.Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO events (service, type, outcome, actor, detail, at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Service, e.Type, e.Outcome, e.Actor, e.Detail, e.At)
	if err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// ListEvents returns events most-recent-first; an empty service returns the whole feed.
func (s *Store) ListEvents(ctx context.Context, service string, limit int) ([]domain.Event, error) {
	query := `SELECT id, service, type, outcome, actor, detail, at FROM events`
	args := []any{}
	if service != "" {
		query += ` WHERE service = ?`
		args = append(args, service)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		if err := rows.Scan(&e.ID, &e.Service, &e.Type, &e.Outcome, &e.Actor, &e.Detail, &e.At); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scan(sc scanner) (domain.Service, error) {
	var (
		svc      domain.Service
		enabled  int
		envJSON  string
		confJSON string
	)
	err := sc.Scan(&svc.Name, &svc.Language, &svc.Replicas, &enabled, &svc.Database.Type, &svc.Status, &svc.RepoURL, &svc.InfraPRURL, &svc.GitopsPRURL, &svc.URL, &envJSON, &confJSON, &svc.ProvisionMs, &svc.CreatedAt)
	if err != nil {
		return domain.Service{}, err
	}
	svc.Database.Enabled = enabled != 0
	if envJSON != "" {
		if err := json.Unmarshal([]byte(envJSON), &svc.Env); err != nil {
			return domain.Service{}, fmt.Errorf("decode env: %w", err)
		}
	}
	if confJSON != "" {
		var rep domain.ConformanceReport
		if err := json.Unmarshal([]byte(confJSON), &rep); err != nil {
			return domain.Service{}, fmt.Errorf("decode conformance: %w", err)
		}
		svc.Conformance = &rep
	}
	return svc, nil
}

func marshalEnv(env []domain.EnvVar) (string, error) {
	if len(env) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("encode env: %w", err)
	}
	return string(b), nil
}

func marshalConformance(rep *domain.ConformanceReport) (string, error) {
	if rep == nil {
		return "", nil
	}
	b, err := json.Marshal(rep)
	if err != nil {
		return "", fmt.Errorf("encode conformance: %w", err)
	}
	return string(b), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
