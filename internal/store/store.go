// Package store defines the catalog persistence (a projection, not the source
// of truth) and an in-memory implementation used in tests.
package store

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/atlas-platform-tcc/atlas-api/internal/domain"
)

var (
	// ErrAlreadyExists is returned when creating a service whose name is taken.
	ErrAlreadyExists = errors.New("service already exists")
	// ErrNotFound is returned when a service is not in the catalog.
	ErrNotFound = errors.New("service not found")
)

// Store is the catalog of services and their projected state.
type Store interface {
	Create(ctx context.Context, s domain.Service) error
	Update(ctx context.Context, s domain.Service) error
	Get(ctx context.Context, name string) (domain.Service, error)
	List(ctx context.Context) ([]domain.Service, error)
}

// Memory is an in-memory Store, useful for tests and local runs without a DB.
type Memory struct {
	mu   sync.RWMutex
	data map[string]domain.Service
}

// NewMemory returns an empty in-memory catalog.
func NewMemory() *Memory {
	return &Memory{data: make(map[string]domain.Service)}
}

func (m *Memory) Create(_ context.Context, s domain.Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[s.Name]; ok {
		return ErrAlreadyExists
	}
	m.data[s.Name] = s
	return nil
}

func (m *Memory) Update(_ context.Context, s domain.Service) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.data[s.Name]; !ok {
		return ErrNotFound
	}
	m.data[s.Name] = s
	return nil
}

func (m *Memory) Get(_ context.Context, name string) (domain.Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.data[name]
	if !ok {
		return domain.Service{}, ErrNotFound
	}
	return s, nil
}

func (m *Memory) List(_ context.Context) ([]domain.Service, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]domain.Service, 0, len(m.data))
	for _, s := range m.data {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
