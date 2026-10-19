package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
	"github.com/atlas-platform-tcc/atlas-api/internal/store"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func sample() domain.Service {
	return domain.Service{
		Name:     "payments",
		Language: "go",
		Replicas: 2,
		Database: domain.Database{Enabled: true, Type: "postgres"},
		Status:   domain.StatusRegistered,
	}
}

func TestCreateAndGet(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	if err := st.Create(ctx, sample()); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := st.Get(ctx, "payments")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "payments" || got.Replicas != 2 || !got.Database.Enabled || got.Database.Type != "postgres" {
		t.Errorf("unexpected: %+v", got)
	}
	if got.CreatedAt == "" {
		t.Error("createdAt should be set")
	}
}

func TestCreateDuplicate(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Create(ctx, sample()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Create(ctx, sample()); err != store.ErrAlreadyExists {
		t.Fatalf("err = %v, want ErrAlreadyExists", err)
	}
}

func TestGetNotFound(t *testing.T) {
	st := newStore(t)
	if _, err := st.Get(context.Background(), "missing"); err != store.ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestDelete(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	if err := st.Create(ctx, sample()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := st.Delete(ctx, "payments"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.Get(ctx, "payments"); err != store.ErrNotFound {
		t.Fatalf("get after delete: err = %v, want ErrNotFound", err)
	}
	// deleting again is a not-found, and the name is free to be recreated
	if err := st.Delete(ctx, "payments"); err != store.ErrNotFound {
		t.Fatalf("delete missing: err = %v, want ErrNotFound", err)
	}
	if err := st.Create(ctx, sample()); err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
}

func TestMigrateFromOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// Create a database with the phase-02 schema (no repo_url, no env_json).
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`CREATE TABLE services (
		name TEXT PRIMARY KEY, language TEXT NOT NULL, replicas INTEGER NOT NULL,
		database_enabled INTEGER NOT NULL, database_type TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL, created_at TEXT NOT NULL)`)
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()

	// Opening via our code must migrate and then work end-to-end.
	st, err := Open(path)
	if err != nil {
		t.Fatalf("open (migrate): %v", err)
	}
	defer st.Close()
	if err := st.Create(context.Background(), sample()); err != nil {
		t.Fatalf("create after migrate: %v", err)
	}
	got, err := st.Get(context.Background(), "payments")
	if err != nil {
		t.Fatalf("get after migrate: %v", err)
	}
	if got.Database.Type != "postgres" {
		t.Errorf("unexpected after migrate: %+v", got)
	}
}

func TestListOrdered(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	for _, n := range []string{"b", "a", "c"} {
		s := sample()
		s.Name = n
		if err := st.Create(ctx, s); err != nil {
			t.Fatalf("create %s: %v", n, err)
		}
	}
	list, err := st.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 || list[0].Name != "a" || list[2].Name != "c" {
		t.Errorf("unexpected order/len: %+v", list)
	}
}
