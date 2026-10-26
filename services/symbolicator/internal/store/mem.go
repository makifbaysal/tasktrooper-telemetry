package store

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"sync"
)

// Mem is an in-memory Objects implementation for tests.
type Mem struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// NewMem returns an empty in-memory store.
func NewMem() *Mem { return &Mem{objs: map[string][]byte{}} }

func (m *Mem) Put(_ context.Context, key string, body io.ReadSeeker, _ int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.objs[key] = data
	m.mu.Unlock()
	return nil
}

func (m *Mem) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.objs[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(d)), nil
}

func (m *Mem) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (m *Mem) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objs, key)
	m.mu.Unlock()
	return nil
}

func (m *Mem) Ping(context.Context) error { return nil }

// Keys returns all stored keys (tests).
func (m *Mem) Keys() []string { k, _ := m.List(context.Background(), ""); return k }
