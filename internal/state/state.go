// Package state is a shard's state on disk: one directory per shard, one file per tenant.
//
//	<state_dir>/shard.json          the shard: its identity, the Snowflake mark, the tenant roster
//	<state_dir>/tenants/<t>.json    one tenant: its name, the same identity, its entities' marks
//
// Every file records which shard it belongs to (Identity). Opening it with a config that
// claims a different shard, cluster size, uuid key or Snowflake epoch is refused, because
// carrying on would issue IDs that collide with another shard's. A tenant file also records
// its tenant, so a file renamed or copied to another tenant is refused. A missing file is
// refused too: silently starting from zero would re-issue every ID handed out before.
//
// A pre-tenant state.json (format 1 or 2) is carried over once, into the tenant the config
// names in legacy_tenant, every mark kept (see Open).
//
// Every change is written to a temp file, fsynced, renamed over the live file and the
// directory is fsynced, so a power loss can never roll the counters back. A change touches
// only its own tenant's file.
package state

import (
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

const formatVersion = 3

// The names inside a shard's state directory.
const (
	ShardFile  = "shard.json"
	TenantsDir = "tenants"
	// LegacyFile is the pre-tenant state file (format 1 or 2), carried over at the first start.
	LegacyFile = "state.json"
	lockFile   = ".lock"

	kindShard  = "shard"
	kindTenant = "tenant"
)

// The scramble a uuid record was minted with. It is stored with the record, never
// re-derived, so an entity keeps its scramble for life and can never repeat a string.
const (
	// TweakTenantEntity: every uuid entity registered since tenancy (tenant + entity name).
	TweakTenantEntity = "tenant+entity"
	// TweakEntity: a uuid entity carried over from a pre-tenant file (entity name alone).
	TweakEntity = "entity"
)

// ErrNotFound is returned by Open when the directory holds no state to open.
var ErrNotFound = errors.New("state not found")

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
	// UUIDTweak names a uuid entity's scramble (TweakTenantEntity or TweakEntity); empty for
	// every other type.
	UUIDTweak string `json:"uuidTweak,omitempty"`
}

// IdentityError explains why a state file does not belong to the running config.
type IdentityError struct {
	Path     string
	Problems []string
}

func (e *IdentityError) Error() string {
	return fmt.Sprintf("state file %s does not belong to this config (%s); using it would issue "+
		"duplicate IDs — point state_dir at this shard's own directory, or fix the config",
		e.Path, strings.Join(e.Problems, "; "))
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

// ---------- one tenant's file ----------

type tenantFile struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Tenant  string `json:"tenant"`
	Identity
	Records []Record `json:"records"`
}

func newTenantFile(tenant string, id Identity) *tenantFile {
	return &tenantFile{Version: formatVersion, Kind: kindTenant, Tenant: tenant, Identity: id, Records: []Record{}}
}

func tenantPath(dir, tenant string) string {
	return filepath.Join(dir, TenantsDir, tenant+".json")
}

// readTenantFile loads tenant's file. It returns nil, nil when the file does not exist.
func readTenantFile(path, tenant string, id Identity) (*tenantFile, error) {
	b, err := readNonEmpty(path)
	if b == nil || err != nil {
		return nil, err
	}
	var f tenantFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("parse state %s: %w", path, err)
	}
	if f.Version != formatVersion || f.Kind != kindTenant {
		return nil, fmt.Errorf("state file %s is not a tenant file of format %d (version %d, kind %q)", path, formatVersion, f.Version, f.Kind)
	}
	if f.Tenant != tenant {
		return nil, fmt.Errorf("state file %s holds tenant %q, not %q: a file renamed or copied from another tenant — "+
			"refusing to serve one tenant's counters as another's", path, f.Tenant, tenant)
	}
	if err := checkIdentity(path, f.Identity, id); err != nil {
		return nil, err
	}
	if err := checkRecords(path, f.Records, id, true); err != nil {
		return nil, err
	}
	if f.Records == nil {
		f.Records = []Record{}
	}
	return &f, nil
}

// checkRecords validates the records of a file. tweaked: the file stores each uuid record's
// scramble (format 3); a pre-tenant file stores none.
func checkRecords(path string, records []Record, id Identity, tweaked bool) error {
	seen := make(map[string]bool, len(records))
	for _, r := range records {
		if seen[r.EntityName] {
			return fmt.Errorf("state file %s lists entity %q twice", path, r.EntityName)
		}
		seen[r.EntityName] = true
		if r.EntityName == "" {
			return fmt.Errorf("state file %s holds an entity without a name", path)
		}
		if _, err := datatype.Parse(string(r.DataType)); err != nil {
			return fmt.Errorf("state file %s, entity %q: %w", path, r.EntityName, err)
		}
		if r.CurrentIteration < 0 {
			return fmt.Errorf("state file %s, entity %q: negative mark %d", path, r.EntityName, r.CurrentIteration)
		}
		if r.ShardID != 0 && r.ShardID != id.ShardID {
			return &IdentityError{Path: path, Problems: []string{
				fmt.Sprintf("entity %q belongs to shard %d, config is shard %d", r.EntityName, r.ShardID, id.ShardID)}}
		}
		switch {
		case !tweaked:
		case r.DataType.IsUUID() && r.UUIDTweak != TweakTenantEntity && r.UUIDTweak != TweakEntity:
			return fmt.Errorf("state file %s, uuid entity %q: unknown scramble %q (want %q or %q) — refusing to guess it",
				path, r.EntityName, r.UUIDTweak, TweakTenantEntity, TweakEntity)
		case !r.DataType.IsUUID() && r.UUIDTweak != "":
			return fmt.Errorf("state file %s, entity %q of type %s carries a uuid scramble", path, r.EntityName, r.DataType)
		}
	}
	return nil
}

// Store is one tenant's entities on one shard.
type Store struct {
	path   string
	tenant string
	id     Identity

	mu      sync.Mutex
	records map[string]*Record
}

func newStore(path string, f *tenantFile) *Store {
	s := &Store{path: path, tenant: f.Tenant, id: f.Identity, records: make(map[string]*Record, len(f.Records))}
	for i := range f.Records {
		r := f.Records[i]
		s.records[r.EntityName] = &r
	}
	return s
}

func (s *Store) Path() string       { return s.path }
func (s *Store) Tenant() string     { return s.tenant }
func (s *Store) Identity() Identity { return s.id }

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
// different types cannot both win. A uuid entity gets the tenant's scramble.
func (s *Store) Register(name string, dt datatype.DataType, startIter int64) (Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.records[name]; ok {
		return *r, false, nil
	}
	r := &Record{EntityName: name, DataType: dt, CurrentIteration: startIter, ShardID: s.id.ShardID}
	if dt.IsUUID() {
		r.UUIDTweak = TweakTenantEntity
	}
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

// Rename gives an entity a new name and carries its mark to it: the next ID under the new
// name is the one the old name would have issued. A uuid entity is refused: its scramble is
// derived from its name, so renaming it could repeat a string.
func (s *Store) Rename(from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[from]
	if !ok {
		return fmt.Errorf("entity %q not found", from)
	}
	if _, taken := s.records[to]; taken {
		return fmt.Errorf("entity %q already exists: renaming %q onto it would give one name two counters", to, from)
	}
	if r.DataType.IsUUID() {
		return fmt.Errorf("entity %q is %s: a uuid entity's scramble is derived from its name, so a new name "+
			"could repeat its strings — refused", from, r.DataType)
	}
	moved := *r
	moved.EntityName = to
	delete(s.records, from)
	s.records[to] = &moved
	if err := s.flush(); err != nil {
		delete(s.records, to)
		s.records[from] = r
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

func (s *Store) sortedLocked() []Record {
	out := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		out = append(out, *r)
	}
	sortRecords(out)
	return out
}

// flush writes the tenant's whole file durably. Caller must hold s.mu.
func (s *Store) flush() error {
	f := newTenantFile(s.tenant, s.id)
	f.Records = s.sortedLocked()
	return writeJSONDurable(s.path, f)
}

func sortRecords(rs []Record) {
	sort.Slice(rs, func(i, j int) bool { return rs[i].EntityName < rs[j].EntityName })
}

// ---------- durable files ----------

// readNonEmpty reads a state file. It returns nil, nil when the file does not exist and
// refuses an empty one (it may have been truncated).
func readNonEmpty(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read state %s: %w", path, err)
	}
	b = []byte(strings.TrimSpace(string(b)))
	if len(b) == 0 {
		return nil, fmt.Errorf("state file %s is empty — refusing to start (it may have been truncated)", path)
	}
	return b, nil
}

// writeJSONDurable writes v to path so that a power loss leaves either the old or the new
// file, never a torn one: temp file, fsync, rename, directory fsync.
func writeJSONDurable(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}
	dir := filepath.Dir(path)
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
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("fsync", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("close temp state: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("rename temp state to %s: %w", path, err)
	}
	return syncDir(dir)
}

// syncDir makes a rename or a new entry in dir durable.
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
