package state

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/telcobright/bucket-next/internal/names"
)

// RosterEntry is one tenant this shard has created a file for. A tenant on the roster whose
// file is missing has lost its file: the shard refuses to start rather than begin it afresh.
type RosterEntry struct {
	Tenant  string `json:"tenant"`
	AddedAt string `json:"addedAt"`
}

// Migration records the last pre-tenant state.json merged into this shard's state.
type Migration struct {
	File   string `json:"file"`
	Format int    `json:"format"`
	Tenant string `json:"tenant"`
	At     string `json:"at"`
}

type shardFile struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Identity
	SnowflakeMarkMs int64         `json:"snowflakeMarkMs"`
	Tenants         []RosterEntry `json:"tenants"`
	MigratedFrom    *Migration    `json:"migratedFrom,omitempty"`
}

func newShardFile(id Identity) *shardFile {
	return &shardFile{Version: formatVersion, Kind: kindShard, Identity: id, Tenants: []RosterEntry{}}
}

func (f *shardFile) onRoster(tenant string) bool {
	for _, e := range f.Tenants {
		if e.Tenant == tenant {
			return true
		}
	}
	return false
}

// readShardFile loads shard.json. It returns nil, nil when the file does not exist.
func readShardFile(path string, id Identity) (*shardFile, error) {
	b, err := readNonEmpty(path)
	if b == nil || err != nil {
		return nil, err
	}
	var f shardFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if f.Version != formatVersion || f.Kind != kindShard {
		return nil, fmt.Errorf("state file %s is not a shard file of format %d (version %d, kind %q)", path, formatVersion, f.Version, f.Kind)
	}
	if err := checkIdentity(path, f.Identity, id); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, e := range f.Tenants {
		if !names.ValidTenant(e.Tenant) || seen[e.Tenant] {
			return nil, fmt.Errorf("state file %s: roster entry %q is malformed or repeated", path, e.Tenant)
		}
		seen[e.Tenant] = true
	}
	if f.Tenants == nil {
		f.Tenants = []RosterEntry{}
	}
	return &f, nil
}

// Shard is the shard-wide state: its identity, the Snowflake mark (the forge is one per
// shard, shared by every tenant) and the tenant roster.
type Shard struct {
	path string
	id   Identity

	mu   sync.Mutex
	file shardFile
}

func newShard(path string, f *shardFile) *Shard {
	return &Shard{path: path, id: f.Identity, file: *f}
}

func (s *Shard) Path() string       { return s.path }
func (s *Shard) Identity() Identity { return s.id }

// SnowflakeMark is the highest timestamp (unix ms) a Snowflake ID may have used.
func (s *Shard) SnowflakeMark() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.SnowflakeMarkMs
}

// RaiseSnowflakeMark persists a new, higher Snowflake mark.
func (s *Shard) RaiseSnowflakeMark(ms int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ms <= s.file.SnowflakeMarkMs {
		return nil
	}
	old := s.file.SnowflakeMarkMs
	s.file.SnowflakeMarkMs = ms
	if err := writeJSONDurable(s.path, &s.file); err != nil {
		s.file.SnowflakeMarkMs = old
		return err
	}
	return nil
}

// Roster returns the tenants this shard has created files for.
func (s *Shard) Roster() []RosterEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RosterEntry(nil), s.file.Tenants...)
}

// MigratedFrom is the last pre-tenant file merged into this shard, or nil.
func (s *Shard) MigratedFrom() *Migration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file.MigratedFrom == nil {
		return nil
	}
	m := *s.file.MigratedFrom
	return &m
}
