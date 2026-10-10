// Package state is the per-shard JSON state file.
//
// The file records which shard it belongs to (Identity). Opening it with a config that
// claims a different shard, cluster size, uuid key or Snowflake epoch is refused, because
// carrying on would issue IDs that collide with another shard's. A missing file is also
// refused: silently starting from zero would re-issue every ID handed out before.
//
// Every change is written to a temp file, fsynced, renamed over the live file and the
// directory is fsynced, so a power loss can never roll the counters back.
package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/telcobright/bucket-next/internal/datatype"
)

const formatVersion = 2

// ErrNotFound is returned by Open when the state file does not exist.
var ErrNotFound = errors.New("state file not found")

// Identity is what a state file belongs to. All four must match the running config.
type Identity struct {
	ShardID            int    `json:"shardId"`
	TotalShards        int    `json:"totalShards"`
	UUIDKeyFingerprint string `json:"uuidKeyFingerprint"`
	SnowflakeEpochMs   int64  `json:"snowflakeEpochMs"`
}

// Record is the on-disk shape for one entity.
//
// For counter types (int, long, uuid*) CurrentIteration is the reserved high-water mark: the
// smallest iteration not yet handed to the service's in-RAM reservation. Everything below it
// may already have been issued. Snowflake entities keep it at 0.
type Record struct {
	EntityName       string            `json:"entityName"`
	DataType         datatype.DataType `json:"dataType"`
	CurrentIteration int64             `json:"currentIteration"`
	ShardID          int               `json:"shardId"`
}

type fileV2 struct {
	Version int `json:"version"`
	Identity
	SnowflakeMarkMs int64    `json:"snowflakeMarkMs"`
	Records         []Record `json:"records"`
}

// IdentityError explains why a state file does not belong to the running config.
type IdentityError struct {
	Path     string
	Problems []string
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("state file %s does not belong to this config (%s); using it would issue "+
		"duplicate IDs — point state_path at this shard's own file, or fix the config",
		e.Path, strings.Join(e.Problems, "; "))
}

type Store struct {
	path     string
	id       Identity
	migrated bool

	mu      sync.Mutex
	sfMark  int64
	records map[string]*Record
}

// Create makes a new, empty state file. It fails if the file already exists.
func Create(path string, id Identity) (*Store, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("state file %s already exists", path)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat state %s: %w", path, err)
	}
	s := &Store{path: path, id: id, records: make(map[string]*Record)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.flush(); err != nil {
		return nil, err
	}
	return s, nil
}

// Open loads an existing state file and checks it belongs to id. A version-1 file (a bare
// JSON array) is accepted if its records name the same shard, and rewritten as version 2.
func Open(path string, id Identity) (*Store, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 {
		return nil, fmt.Errorf("state file %s is empty — refusing to start (it may have been truncated)", path)
	}
	s := &Store{path: path, id: id, records: make(map[string]*Record)}

	if b[0] == '[' {
		var arr []Record
		if err := json.Unmarshal(b, &arr); err != nil {
			return nil, fmt.Errorf("parse state %s: %w", path, err)
		}
		for i := range arr {
			r := arr[i]
			if r.ShardID != 0 && r.ShardID != id.ShardID {
				return nil, &IdentityError{Path: path, Problems: []string{
					fmt.Sprintf("entity %q belongs to shard %d, config is shard %d", r.EntityName, r.ShardID, id.ShardID)}}
			}
			r.ShardID = id.ShardID
			s.records[r.EntityName] = &r
		}
		s.migrated = true
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := s.flush(); err != nil {
			return nil, err
		}
		return s, nil
	}

	var f fileV2
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if f.Version != formatVersion {
		return nil, fmt.Errorf("state file %s has version %d, this build reads version %d", path, f.Version, formatVersion)
	}
	if err := checkIdentity(path, f.Identity, id); err != nil {
		return nil, err
	}
	s.sfMark = f.SnowflakeMarkMs
	for i := range f.Records {
		r := f.Records[i]
		s.records[r.EntityName] = &r
	}
	return s, nil
}

func checkIdentity(path string, got, want Identity) error {
	var p []string
	if got.ShardID != want.ShardID {
		p = append(p, fmt.Sprintf("shard_id: file=%d config=%d", got.ShardID, want.ShardID))
	}
	if got.TotalShards != want.TotalShards {
		p = append(p, fmt.Sprintf("total_shards: file=%d config=%d", got.TotalShards, want.TotalShards))
	}
	if got.UUIDKeyFingerprint != want.UUIDKeyFingerprint {
		p = append(p, fmt.Sprintf("uuid_key changed: file fingerprint=%s config fingerprint=%s",
			got.UUIDKeyFingerprint, want.UUIDKeyFingerprint))
	}
	if got.SnowflakeEpochMs != want.SnowflakeEpochMs {
		p = append(p, fmt.Sprintf("snowflake_epoch_ms: file=%d config=%d", got.SnowflakeEpochMs, want.SnowflakeEpochMs))
	}
	if len(p) > 0 {
		return &IdentityError{Path: path, Problems: p}
	}
	return nil
}

func (s *Store) Path() string         { return s.path }
func (s *Store) Identity() Identity   { return s.id }
func (s *Store) MigratedFromV1() bool { return s.migrated }

// Get returns a copy of the record.
func (s *Store) Get(name string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return Record{}, false
	}
	return *r, true
}

// All returns a copy of every record, sorted by name.
func (s *Store) All() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sortedLocked()
}

// Register creates the entity with the given starting mark, or returns the existing record
// untouched (created=false). Check-and-create is atomic, so two first requests with
// different types cannot both win.
func (s *Store) Register(name string, dt datatype.DataType, startIter int64) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[name]; ok {
		return *r, false, nil
	}
	r := &Record{EntityName: name, DataType: dt, CurrentIteration: startIter, ShardID: s.id.ShardID}
	s.records[name] = r
	if err := s.flush(); err != nil {
		delete(s.records, name)
		return Record{}, false, err
	}
	return *r, true, nil
}

// RaiseMark moves the entity's mark up to iter. A lower iter is a no-op: marks only move
// down through SetMark.
func (s *Store) RaiseMark(name string, iter int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return fmt.Errorf("entity %q not found", name)
	}
	if iter <= r.CurrentIteration {
		return nil
	}
	old := r.CurrentIteration
	r.CurrentIteration = iter
	if err := s.flush(); err != nil {
		r.CurrentIteration = old
		return err
	}
	return nil
}

// SetMark sets the entity's mark exactly. Used by reset, after the caller has checked it is safe.
func (s *Store) SetMark(name string, iter int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return fmt.Errorf("entity %q not found", name)
	}
	old := r.CurrentIteration
	r.CurrentIteration = iter
	if err := s.flush(); err != nil {
		r.CurrentIteration = old
		return err
	}
	return nil
}

// Delete removes one entity.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[name]
	if !ok {
		return fmt.Errorf("entity %q not found", name)
	}
	delete(s.records, name)
	if err := s.flush(); err != nil {
		s.records[name] = r
		return err
	}
	return nil
}

// Clear removes every entity.
func (s *Store) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.records
	s.records = make(map[string]*Record)
	if err := s.flush(); err != nil {
		s.records = old
		return err
	}
	return nil
}

// SnowflakeMark is the highest timestamp (unix ms) a Snowflake ID may have used.
func (s *Store) SnowflakeMark() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sfMark
}

// RaiseSnowflakeMark persists a new, higher Snowflake mark.
func (s *Store) RaiseSnowflakeMark(ms int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms <= s.sfMark {
		return nil
	}
	old := s.sfMark
	s.sfMark = ms
	if err := s.flush(); err != nil {
		s.sfMark = old
		return err
	}
	return nil
}

func (s *Store) sortedLocked() []Record {
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EntityName < out[j].EntityName })
	return out
}

// flush writes the whole file durably. Caller must hold s.mu.
func (s *Store) flush() error {
	b, err := json.MarshalIndent(fileV2{
		Version:         formatVersion,
		Identity:        s.id,
		SnowflakeMarkMs: s.sfMark,
		Records:         s.sortedLocked(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create state dir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".state-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp state: %w", err)
	}
	fail := func(step string, err error) error {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("%s temp state: %w", step, err)
	}
	if _, err := tmp.Write(b); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("fsync", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename temp state to %s: %w", s.path, err)
	}
	return syncDir(dir)
}

// syncDir makes the rename itself durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open state dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync state dir: %w", err)
	}
	return nil
}
