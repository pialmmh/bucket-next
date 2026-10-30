package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/telcobright/bucket-next/internal/datatype"
)

var testID = Identity{ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000}

// bedID is the bed's identity: shard 1 of a declared 2.
var bedID = Identity{ShardID: 1, TotalShards: 2, UUIDKeyFingerprint: "987a907e1b019511", SnowflakeEpochMs: 1704067200000}

func opts(dir string, tenants ...string) Options {
	return Options{Dir: dir, Identity: testID, Tenants: tenants}
}

// mustOpen opens o and closes the layout when the test ends (Close is idempotent).
func mustOpen(t *testing.T, o Options) *Layout {
	t.Helper()
	l, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func newShardDir(t *testing.T, tenants ...string) (*Layout, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "shard-2")
	o := opts(dir, tenants...)
	o.Create = true
	return mustOpen(t, o), dir
}

// reopen closes l (releasing the lock) and opens o, as a restart would.
func reopen(t *testing.T, l *Layout, o Options) *Layout {
	t.Helper()
	l.Close()
	return mustOpen(t, o)
}

func writeV2(t *testing.T, dir string, id Identity, sfMark int64, recs ...Record) []byte {
	t.Helper()
	b, err := json.MarshalIndent(fileV2{Version: 2, Identity: id, SnowflakeMarkMs: sfMark, Records: recs}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, LegacyFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

func noteWith(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}

// ---------- a fresh shard ----------

func TestOpen_nothingOnDiskIsNotFound(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(opts(dir, "btcl")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty dir: want ErrNotFound, got %v", err)
	}
	if _, err := Open(opts(filepath.Join(dir, "absent"), "btcl")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing dir: want ErrNotFound, got %v", err)
	}
}

func TestOpen_createMakesTheShardAndEveryTenant(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	for _, f := range []string{ShardFile, "tenants/btcl.json", "tenants/acme.json"} {
		if !exists(filepath.Join(dir, f)) {
			t.Errorf("%s not created", f)
		}
	}
	roster := l.Shard.Roster()
	if len(roster) != 2 || roster[0].Tenant != "btcl" || roster[1].Tenant != "acme" {
		t.Errorf("roster = %+v", roster)
	}
	if len(l.Tenants) != 2 || l.Tenants["btcl"].Tenant() != "btcl" {
		t.Errorf("tenants = %v", l.Tenants)
	}
	if !noteWith(l.Notes, "brand-new shard 2/3") {
		t.Errorf("notes = %q", l.Notes)
	}
}

func TestOptions_areChecked(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]Options{
		"no tenant":        {Dir: dir, Identity: testID},
		"bad tenant":       {Dir: dir, Identity: testID, Tenants: []string{"BTCL"}},
		"twice":            {Dir: dir, Identity: testID, Tenants: []string{"btcl", "btcl"}},
		"legacy unknown":   {Dir: dir, Identity: testID, Tenants: []string{"btcl"}, LegacyTenant: "acme"},
		"adopt undeclared": {Dir: dir, Identity: testID, Tenants: []string{"btcl"}, Adopt: []string{"acme"}},
	}
	for name, o := range cases {
		if _, err := Open(o); err == nil || errors.Is(err, ErrNotFound) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
}

func TestOpen_lockRefusesASecondOpener(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	if _, err := Open(opts(dir, "btcl")); err == nil || !strings.Contains(err.Error(), "another process holds") {
		t.Fatalf("second opener: want the lock refusal, got %v", err)
	}
	l.Close()
	mustOpen(t, opts(dir, "btcl"))
}

// ---------- one tenant's store ----------

func TestStore_registerPersistsInItsTenantOnly(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	acmeBefore, _ := os.ReadFile(filepath.Join(dir, "tenants/acme.json"))
	r, created, err := l.Tenants["btcl"].Register("cdr", datatype.Long, 7)
	if err != nil || !created || r.CurrentIteration != 7 || r.ShardID != 2 {
		t.Fatalf("first Register: %+v created=%v err=%v", r, created, err)
	}
	r, created, _ = l.Tenants["btcl"].Register("cdr", datatype.Int, 99)
	if created || r.DataType != datatype.Long || r.CurrentIteration != 7 {
		t.Fatalf("second Register must return the existing record: %+v created=%v", r, created)
	}
	if acmeAfter, _ := os.ReadFile(filepath.Join(dir, "tenants/acme.json")); !bytes.Equal(acmeBefore, acmeAfter) {
		t.Error("a write for btcl touched acme's file")
	}
	l = reopen(t, l, opts(dir, "btcl", "acme"))
	if got, ok := l.Tenants["btcl"].Get("cdr"); !ok || got.CurrentIteration != 7 {
		t.Errorf("btcl after reopen: %+v %v", got, ok)
	}
	if _, ok := l.Tenants["acme"].Get("cdr"); ok {
		t.Error("acme sees btcl's entity")
	}
}

func TestStore_concurrentFirstRegistersOneWinner(t *testing.T) {
	l, _ := newShardDir(t, "btcl")
	s := l.Tenants["btcl"]
	var wg sync.WaitGroup
	wins := make(chan datatype.DataType, 20)
	for i := 0; i < 20; i++ {
		dt := datatype.Long
		if i%2 == 0 {
			dt = datatype.Int
		}
		wg.Add(1)
		go func(dt datatype.DataType) {
			defer wg.Done()
			if _, created, _ := s.Register("x", dt, 0); created {
				wins <- dt
			}
		}(dt)
	}
	wg.Wait()
	close(wins)
	if n := len(wins); n != 1 {
		t.Fatalf("%d winners, want 1", n)
	}
}

func TestStore_marks(t *testing.T) {
	l, _ := newShardDir(t, "btcl")
	s := l.Tenants["btcl"]
	s.Register("x", datatype.Long, 0)
	s.RaiseMark("x", 100)
	s.RaiseMark("x", 50)
	if r, _ := s.Get("x"); r.CurrentIteration != 100 {
		t.Errorf("mark=%d, want 100", r.CurrentIteration)
	}
	s.SetMark("x", 10)
	if r, _ := s.Get("x"); r.CurrentIteration != 10 {
		t.Errorf("SetMark: mark=%d, want 10", r.CurrentIteration)
	}
	if err := s.RaiseMark("missing", 1); err == nil {
		t.Error("RaiseMark on a missing entity must fail")
	}
}

func TestStore_deleteClearAndSorted(t *testing.T) {
	l, _ := newShardDir(t, "btcl")
	s := l.Tenants["btcl"]
	for _, n := range []string{"c", "a", "b"} {
		s.Register(n, datatype.Int, 0)
	}
	if all := s.All(); all[0].EntityName != "a" || all[2].EntityName != "c" {
		t.Errorf("not sorted: %+v", all)
	}
	if err := s.Delete("a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("a"); ok {
		t.Error("a still present")
	}
	if err := s.Delete("a"); err == nil {
		t.Error("deleting a missing entity must fail")
	}
	s.Clear()
	if n := len(s.All()); n != 0 {
		t.Errorf("after Clear: %d records", n)
	}
}

func TestStore_uuidEntitiesGetTheTenantsScramble(t *testing.T) {
	l, _ := newShardDir(t, "btcl")
	u, _, _ := l.Tenants["btcl"].Register("tok", datatype.UUID12, 0)
	n, _, _ := l.Tenants["btcl"].Register("cdr", datatype.Long, 0)
	if u.UUIDTweak != TweakTenantEntity || n.UUIDTweak != "" {
		t.Errorf("uuid tweak %q, long tweak %q", u.UUIDTweak, n.UUIDTweak)
	}
}

func TestStore_renameCarriesTheMark(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	s := l.Tenants["btcl"]
	s.Register("partner.btcl", datatype.Int, 1003)
	s.Register("taken", datatype.Int, 0)
	s.Register("tok", datatype.UUID16, 5)
	if err := s.Rename("partner.btcl", "taken"); err == nil {
		t.Error("rename onto an existing entity must fail")
	}
	if err := s.Rename("tok", "tok2"); err == nil || !strings.Contains(err.Error(), "scramble") {
		t.Errorf("uuid rename: want the scramble refusal, got %v", err)
	}
	if err := s.Rename("missing", "x"); err == nil {
		t.Error("rename of a missing entity must fail")
	}
	if err := s.Rename("partner.btcl", "partner"); err != nil {
		t.Fatal(err)
	}
	l = reopen(t, l, opts(dir, "btcl"))
	if _, ok := l.Tenants["btcl"].Get("partner.btcl"); ok {
		t.Error("the old name is still there")
	}
	if r, ok := l.Tenants["btcl"].Get("partner"); !ok || r.CurrentIteration != 1003 || r.DataType != datatype.Int {
		t.Errorf("renamed record: %+v %v", r, ok)
	}
}

func TestShard_snowflakeMarkPersists(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Shard.RaiseSnowflakeMark(5000)
	l.Shard.RaiseSnowflakeMark(4000)
	l = reopen(t, l, opts(dir, "btcl"))
	if got := l.Shard.SnowflakeMark(); got != 5000 {
		t.Errorf("mark=%d, want 5000", got)
	}
}

func TestStore_writesLeaveNoTempFiles(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Tenants["btcl"].Register("x", datatype.Int, 0)
	l.Shard.RaiseSnowflakeMark(1)
	for _, d := range []string{dir, filepath.Join(dir, TenantsDir)} {
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".tmp") {
				t.Errorf("temp file left: %s", filepath.Join(d, e.Name()))
			}
		}
	}
}

// ---------- refusals of a shard directory ----------

func TestOpen_identityMismatchIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Close()
	cases := map[string]Identity{
		"shard_id":           {ShardID: 1, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000},
		"total_shards":       {ShardID: 2, TotalShards: 4, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000},
		"uuid_key":           {ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "other", SnowflakeEpochMs: 1704067200000},
		"snowflake_epoch_ms": {ShardID: 2, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1},
	}
	for field, id := range cases {
		o := opts(dir, "btcl")
		o.Identity = id
		_, err := Open(o)
		var ie *IdentityError
		if !errors.As(err, &ie) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: want an IdentityError naming it, got %v", field, err)
		}
	}
	mustOpen(t, opts(dir, "btcl"))
}

func TestOpen_aTenantFileOfAnotherShardIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Close()
	other := newTenantFile("btcl", Identity{ShardID: 1, TotalShards: 3, UUIDKeyFingerprint: "abc123", SnowflakeEpochMs: 1704067200000})
	if err := writeJSONDurable(tenantPath(dir, "btcl"), other); err != nil {
		t.Fatal(err)
	}
	var ie *IdentityError
	if _, err := Open(opts(dir, "btcl")); !errors.As(err, &ie) {
		t.Fatalf("want an IdentityError, got %v", err)
	}
}

func TestOpen_aFileCopiedToAnotherTenantIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	l.Tenants["btcl"].Register("partner.btcl", datatype.Int, 1003)
	l.Close()
	b, _ := os.ReadFile(tenantPath(dir, "btcl"))
	os.WriteFile(tenantPath(dir, "acme"), b, 0o600)
	if _, err := Open(opts(dir, "btcl", "acme")); err == nil || !strings.Contains(err.Error(), `holds tenant "btcl", not "acme"`) {
		t.Fatalf("want the other-tenant refusal, got %v", err)
	}
}

func TestOpen_aLostTenantFileIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	l.Close()
	os.Remove(tenantPath(dir, "acme"))
	if _, err := Open(opts(dir, "btcl", "acme")); err == nil || !strings.Contains(err.Error(), "it was lost") {
		t.Fatalf("want the lost-file refusal, got %v", err)
	}
}

func TestOpen_emptyOrTruncatedFilesAreRefused(t *testing.T) {
	for _, f := range []string{ShardFile, "tenants/btcl.json"} {
		l, dir := newShardDir(t, "btcl")
		l.Close()
		os.WriteFile(filepath.Join(dir, f), []byte("  \n"), 0o600)
		if _, err := Open(opts(dir, "btcl")); err == nil || !strings.Contains(err.Error(), "is empty") {
			t.Errorf("%s empty: want a refusal, got %v", f, err)
		}
	}
}

func TestOpen_aMissingShardFileWithTenantFilesIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Close()
	os.Remove(filepath.Join(dir, ShardFile))
	o := opts(dir, "btcl")
	o.Create = true
	if _, err := Open(o); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("want the incomplete-state refusal (even with Create), got %v", err)
	}
}

func TestOpen_aUuidRecordWithoutItsScrambleIsRefused(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l.Close()
	f := newTenantFile("btcl", testID)
	f.Records = []Record{{EntityName: "tok", DataType: datatype.UUID16, CurrentIteration: 3, ShardID: 2}}
	writeJSONDurable(tenantPath(dir, "btcl"), f)
	if _, err := Open(opts(dir, "btcl")); err == nil || !strings.Contains(err.Error(), "refusing to guess") {
		t.Fatalf("want the scramble refusal, got %v", err)
	}
}

// ---------- the roster ----------

func TestOpen_aNewTenantJoinsTheRoster(t *testing.T) {
	l, dir := newShardDir(t, "btcl")
	l = reopen(t, l, opts(dir, "btcl", "acme"))
	if r := l.Shard.Roster(); len(r) != 2 || r[1].Tenant != "acme" {
		t.Errorf("roster = %+v", r)
	}
	if !exists(tenantPath(dir, "acme")) || !noteWith(l.Notes, "create tenant acme") {
		t.Errorf("acme not created; notes %q", l.Notes)
	}
}

func TestOpen_anUnrosteredFileWithEntitiesNeedsAdoption(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	l.Tenants["acme"].Register("cdr", datatype.Long, 40)
	l.Close()
	// shard.json restored from a backup older than acme: the roster lacks it
	sf, _ := readShardFile(filepath.Join(dir, ShardFile), testID)
	sf.Tenants = sf.Tenants[:1]
	writeJSONDurable(filepath.Join(dir, ShardFile), sf)

	if _, err := Open(opts(dir, "btcl", "acme")); err == nil || !strings.Contains(err.Error(), "adopt-tenant acme") {
		t.Fatalf("want the adoption refusal, got %v", err)
	}
	o := opts(dir, "btcl", "acme")
	o.Adopt = []string{"acme"}
	l = mustOpen(t, o)
	if r, _ := l.Tenants["acme"].Get("cdr"); r.CurrentIteration != 40 {
		t.Errorf("adopted mark = %d", r.CurrentIteration)
	}
	l = reopen(t, l, opts(dir, "btcl", "acme")) // on the roster now
	if len(l.Shard.Roster()) != 2 {
		t.Errorf("roster = %+v", l.Shard.Roster())
	}
}

func TestOpen_anEmptyUnrosteredFileIsAdopted(t *testing.T) {
	// a crash between a new tenant's file and the roster's update
	l, dir := newShardDir(t, "btcl")
	l.Close()
	writeJSONDurable(tenantPath(dir, "acme"), newTenantFile("acme", testID))
	l = mustOpen(t, opts(dir, "btcl", "acme"))
	if !noteWith(l.Notes, "put tenant acme's existing file (0 entities) on the roster") {
		t.Errorf("notes = %q", l.Notes)
	}
}

func TestOpen_anUndeclaredTenantIsKeptAndNotServed(t *testing.T) {
	l, dir := newShardDir(t, "btcl", "acme")
	l.Tenants["acme"].Register("cdr", datatype.Long, 40)
	before, _ := os.ReadFile(tenantPath(dir, "acme"))
	l = reopen(t, l, opts(dir, "btcl"))
	if _, served := l.Tenants["acme"]; served || !noteWith(l.Notes, "tenant acme is on the roster but not in tenants") {
		t.Errorf("acme served=%v; notes %q", served, l.Notes)
	}
	if after, _ := os.ReadFile(tenantPath(dir, "acme")); !bytes.Equal(before, after) {
		t.Error("acme's file changed")
	}
	l = reopen(t, l, opts(dir, "btcl", "acme")) // declared again: its counters resume
	if r, _ := l.Tenants["acme"].Get("cdr"); r.CurrentIteration != 40 {
		t.Errorf("acme resumed at %d", r.CurrentIteration)
	}
}

// ---------- the carry-over of a pre-tenant state.json ----------

func bedOpts(dir string) Options {
	return Options{Dir: dir, Identity: bedID, Tenants: []string{"btcl"}, LegacyTenant: "btcl"}
}

func bedRecord() Record {
	return Record{EntityName: "partner.btcl", DataType: datatype.Int, CurrentIteration: 1003, ShardID: 1}
}

func TestMigrate_theBedsFileKeepsEveryMark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shard-1")
	original := writeV2(t, dir, bedID, 1761577599000, bedRecord())
	l := mustOpen(t, bedOpts(dir))

	r, ok := l.Tenants["btcl"].Get("partner.btcl")
	if !ok || r.CurrentIteration != 1003 || r.DataType != datatype.Int || r.ShardID != 1 || r.UUIDTweak != "" {
		t.Fatalf("migrated record: %+v %v", r, ok)
	}
	if l.Shard.SnowflakeMark() != 1761577599000 {
		t.Errorf("snowflake mark = %d", l.Shard.SnowflakeMark())
	}
	if m := l.Shard.MigratedFrom(); m == nil || m.Tenant != "btcl" || m.Format != 2 || m.File != LegacyFile {
		t.Errorf("migratedFrom = %+v", m)
	}
	if r := l.Shard.Roster(); len(r) != 1 || r[0].Tenant != "btcl" {
		t.Errorf("roster = %+v", r)
	}
	if exists(filepath.Join(dir, LegacyFile)) {
		t.Error("state.json still in place")
	}
	if kept, _ := os.ReadFile(filepath.Join(dir, LegacyFile+".migrated-v2")); !bytes.Equal(kept, original) {
		t.Error("state.json.migrated-v2 is not the original file")
	}
	if !noteWith(l.Notes, "carry the pre-tenant state.json (format 2, 1 entities) over into tenant btcl") {
		t.Errorf("notes = %q", l.Notes)
	}
	l = reopen(t, l, bedOpts(dir)) // the second start opens format 3 as it is
	if r, _ := l.Tenants["btcl"].Get("partner.btcl"); r.CurrentIteration != 1003 {
		t.Errorf("after restart mark = %d", r.CurrentIteration)
	}
}

func TestMigrate_formatOne(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, LegacyFile), []byte(`[{"entityName":"cdr","dataType":"long","currentIteration":40,"shardId":2}]`), 0o600)
	o := opts(dir, "btcl")
	o.LegacyTenant = "btcl"
	l := mustOpen(t, o)
	if r, _ := l.Tenants["btcl"].Get("cdr"); r.CurrentIteration != 40 {
		t.Errorf("mark = %d", r.CurrentIteration)
	}
	if !exists(filepath.Join(dir, LegacyFile+".migrated-v1")) {
		t.Error("not renamed to state.json.migrated-v1")
	}
}

func TestMigrate_withoutAnOwnerIsRefusedAndChangesNothing(t *testing.T) {
	dir := t.TempDir()
	original := writeV2(t, dir, bedID, 1, bedRecord())
	o := bedOpts(dir)
	o.LegacyTenant = ""
	if _, err := Open(o); err == nil || !strings.Contains(err.Error(), "legacy_tenant") {
		t.Fatalf("want the owner refusal, got %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, LegacyFile)); !bytes.Equal(b, original) {
		t.Error("state.json changed")
	}
	if exists(filepath.Join(dir, ShardFile)) || exists(filepath.Join(dir, TenantsDir)) {
		t.Error("a refused start wrote state")
	}
}

func TestMigrate_anotherShardsFileIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, Identity{ShardID: 2, TotalShards: 2, UUIDKeyFingerprint: bedID.UUIDKeyFingerprint, SnowflakeEpochMs: bedID.SnowflakeEpochMs}, 1)
	var ie *IdentityError
	if _, err := Open(bedOpts(dir)); !errors.As(err, &ie) {
		t.Fatalf("want an IdentityError, got %v", err)
	}
	if exists(filepath.Join(dir, ShardFile)) {
		t.Error("a refused start wrote shard.json")
	}
}

func TestMigrate_uuidEntitiesKeepTheirOldScramble(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 1, Record{EntityName: "tok", DataType: datatype.UUID16, CurrentIteration: 9, ShardID: 1})
	l := mustOpen(t, bedOpts(dir))
	if r, _ := l.Tenants["btcl"].Get("tok"); r.UUIDTweak != TweakEntity {
		t.Errorf("tweak = %q, want %q", r.UUIDTweak, TweakEntity)
	}
}

func TestMigrate_aCrashAfterTheTenantFileReRuns(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	// the first start wrote the tenant file and died before shard.json
	tf := newTenantFile("btcl", bedID)
	tf.Records = []Record{bedRecord()}
	writeJSONDurable(tenantPath(dir, "btcl"), tf)

	l := mustOpen(t, bedOpts(dir))
	if r, _ := l.Tenants["btcl"].Get("partner.btcl"); r.CurrentIteration != 1003 {
		t.Errorf("mark = %d", r.CurrentIteration)
	}
	if !exists(filepath.Join(dir, ShardFile)) || exists(filepath.Join(dir, LegacyFile)) {
		t.Error("the re-run did not finish the carry-over")
	}
}

func TestMigrate_aCrashBeforeTheRenameReRuns(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	l := mustOpen(t, bedOpts(dir))
	l.Close()
	// the first start wrote both files and died before renaming state.json away
	b, _ := os.ReadFile(filepath.Join(dir, LegacyFile+".migrated-v2"))
	os.WriteFile(filepath.Join(dir, LegacyFile), b, 0o600)

	l = mustOpen(t, bedOpts(dir))
	if !noteWith(l.Notes, "nothing moves") || exists(filepath.Join(dir, LegacyFile)) {
		t.Errorf("leftover not retired; notes %q", l.Notes)
	}
	if r, _ := l.Tenants["btcl"].Get("partner.btcl"); r.CurrentIteration != 1003 {
		t.Errorf("mark = %d", r.CurrentIteration)
	}
}

func TestMigrate_anOlderBinaryThatRanAfterIsMergedForward(t *testing.T) {
	for _, legacy := range []string{"btcl", ""} { // "": the owner comes from shard.json's migratedFrom
		dir := t.TempDir()
		writeV2(t, dir, bedID, 7, bedRecord())
		l := mustOpen(t, bedOpts(dir))
		l.Tenants["btcl"].RaiseMark("partner.btcl", 2003)
		l.Close()
		ahead := bedRecord()
		ahead.CurrentIteration = 3003
		writeV2(t, dir, bedID, 9, ahead, Record{EntityName: "sms", DataType: datatype.Long, CurrentIteration: 5, ShardID: 1})

		o := bedOpts(dir)
		o.LegacyTenant = legacy
		l = mustOpen(t, o)
		if r, _ := l.Tenants["btcl"].Get("partner.btcl"); r.CurrentIteration != 3003 {
			t.Errorf("legacy=%q: mark = %d, want 3003", legacy, r.CurrentIteration)
		}
		if _, ok := l.Tenants["btcl"].Get("sms"); !ok || !noteWith(l.Notes, "WARNING") {
			t.Errorf("legacy=%q: sms not merged or no warning; notes %q", legacy, l.Notes)
		}
		l.Close()
	}
}

func TestMigrate_aLowerMarkNeverMovesBack(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	tf := newTenantFile("btcl", bedID)
	r := bedRecord()
	r.CurrentIteration = 5003
	tf.Records = []Record{r}
	writeJSONDurable(tenantPath(dir, "btcl"), tf)
	l := mustOpen(t, bedOpts(dir))
	if got, _ := l.Tenants["btcl"].Get("partner.btcl"); got.CurrentIteration != 5003 {
		t.Errorf("mark = %d, want 5003", got.CurrentIteration)
	}
}

func TestMigrate_aTypeConflictIsRefused(t *testing.T) {
	dir := t.TempDir()
	r := bedRecord()
	r.DataType = datatype.Long
	writeV2(t, dir, bedID, 7, r)
	tf := newTenantFile("btcl", bedID)
	tf.Records = []Record{bedRecord()}
	writeJSONDurable(tenantPath(dir, "btcl"), tf)
	if _, err := Open(bedOpts(dir)); err == nil || !strings.Contains(err.Error(), "different types") {
		t.Fatalf("want the type refusal, got %v", err)
	}
}

func TestMigrate_offlineToolsAreRefused(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	o := bedOpts(dir)
	o.NoMigrate = true
	if _, err := Open(o); err == nil || !strings.Contains(err.Error(), "refusing to change the state offline") {
		t.Fatalf("want the offline refusal, got %v", err)
	}
}

// ---------- Inspect ----------

func TestInspect_readsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	v, err := Inspect(bedOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Tenants["btcl"]) != 1 || v.Tenants["btcl"][0].CurrentIteration != 1003 || !noteWith(v.Notes, "carry the pre-tenant") {
		t.Errorf("view = %+v", v)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("Inspect wrote: %d entries in the dir", len(entries))
	}
	o := bedOpts(dir)
	o.LegacyTenant = ""
	if _, err := Inspect(o); err == nil {
		t.Error("Inspect must report the refusal the start would meet")
	}
}

// ---------- the route back ----------

func TestExportV2_theRouteBackAndForward(t *testing.T) {
	dir := t.TempDir()
	writeV2(t, dir, bedID, 7, bedRecord())
	l := mustOpen(t, bedOpts(dir))
	l.Tenants["btcl"].RaiseMark("partner.btcl", 2003)
	l.Shard.RaiseSnowflakeMark(99)

	path, err := l.ExportV2("btcl")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
	lf, err := readLegacy(path, bedID)
	if err != nil || lf.Format != 2 || lf.SfMark != 99 || len(lf.Records) != 1 || lf.Records[0].CurrentIteration != 2003 {
		t.Fatalf("exported file: %+v %v", lf, err)
	}
	if exists(filepath.Join(dir, ShardFile)) {
		t.Error("shard.json not retired")
	}
	// the old binary runs and moves the mark on, then this binary starts again
	moved := bedRecord()
	moved.CurrentIteration = 4003
	writeV2(t, dir, bedID, 120, moved)
	l = mustOpen(t, bedOpts(dir))
	if r, _ := l.Tenants["btcl"].Get("partner.btcl"); r.CurrentIteration != 4003 {
		t.Errorf("after the round trip mark = %d, want 4003", r.CurrentIteration)
	}
	if l.Shard.SnowflakeMark() != 120 {
		t.Errorf("snowflake mark = %d", l.Shard.SnowflakeMark())
	}
}

func TestExportV2_refusals(t *testing.T) {
	l, _ := newShardDir(t, "btcl", "acme")
	l.Tenants["acme"].Register("cdr", datatype.Long, 1)
	if _, err := l.ExportV2("btcl"); err == nil || !strings.Contains(err.Error(), "strand") {
		t.Errorf("another tenant with entities: want a refusal, got %v", err)
	}
	l.Tenants["acme"].Clear()
	l.Tenants["btcl"].Register("tok", datatype.UUID8, 0)
	if _, err := l.ExportV2("btcl"); err == nil || !strings.Contains(err.Error(), "scramble") {
		t.Errorf("a tenant-scrambled uuid entity: want a refusal, got %v", err)
	}
}
