package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/telcobright/bucket-next/internal/datatype"
)

// Record is the on-disk shape for one entity.
//
// CurrentIteration is the reserved high-water mark — the smallest iteration that has NOT
// yet been allocated to a segment. The Service emits values from in-RAM segments and only
// re-writes this when reserving a new segment. The CLI manipulates it directly.
type Record struct {
	EntityName       string            `json:"entityName"`
	DataType         datatype.DataType `json:"dataType"`
	CurrentIteration int64             `json:"currentIteration"`
	ShardID          int               `json:"shardId"`
}

// Store is the JSON file on disk plus an in-memory map. All access goes through methods.
type Store struct {
	path    string
	mu      sync.Mutex
	records map[string]*Record
}

// Open loads the store from disk, creating an empty in-memory map if the file does not exist.
func Open(path string) (*Store, error) {
	s := &Store{
		path:    path,
		records: make(map[string]*Record),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read state %q: %w", s.path, err)
	}
	if len(b) == 0 {
		return nil
	}
	var arr []Record
	if err := json.Unmarshal(b, &arr); err != nil {
		return fmt.Errorf("parse state %q: %w", s.path, err)
	}
	for i := range arr {
		r := arr[i]
		s.records[r.EntityName] = &r
	}
	return nil
}

// Get returns a copy of the record (or false if not present).
func (s *Store) Get(name string) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return nil, false
	}
	c := *r
	return &c, true
}

// All returns a copy of every record.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	return out
}

// Put writes-or-replaces a record and flushes to disk.
func (s *Store) Put(r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := r
	s.records[r.EntityName] = &c
	return s.flush()
}

// Delete removes one record and flushes.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, name)
	return s.flush()
}

// Clear removes all records and flushes.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = make(map[string]*Record)
	return s.flush()
}

// flush serialises all records and atomically replaces the on-disk file.
// Caller must hold s.mu.
func (s *Store) flush() error {
	arr := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		arr = append(arr, *r)
	}
	b, err := json.MarshalIndent(arr, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create state dir %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename temp state to %q: %w", s.path, err)
	}
	return nil
}
