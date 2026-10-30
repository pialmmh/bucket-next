package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/telcobright/bucket-next/internal/names"
)

// Options says which shard state to open and what the config declares.
type Options struct {
	Dir      string   // the shard's state directory
	Identity Identity // what every file must belong to
	// Tenants are the tenants the config declares, and so serves. At least one.
	Tenants []string
	// LegacyTenant owns the counters of a pre-tenant state.json found in Dir. Empty: such a
	// file is refused (never guessed), unless shard.json names the tenant of an earlier merge.
	LegacyTenant string
	// Create makes the state of a brand-new shard when Dir holds none.
	Create bool
	// NoMigrate refuses a pre-tenant state.json instead of merging it (offline tools: the
	// running service may still be an older binary writing that file).
	NoMigrate bool
	// Adopt puts these tenants' files on the roster although they are not on it (after
	// shard.json was restored from an older backup than the tenant's file).
	Adopt []string
}

func (o Options) check() error {
	if o.Dir == "" {
		return errors.New("state: no directory given")
	}
	if len(o.Tenants) == 0 {
		return errors.New("state: no tenant declared")
	}
	seen := map[string]bool{}
	for _, t := range o.Tenants {
		if !names.ValidTenant(t) {
			return fmt.Errorf("state: tenant %q breaks the rule %s", t, names.TenantRule)
		}
		if seen[t] {
			return fmt.Errorf("state: tenant %q is declared twice", t)
		}
		seen[t] = true
	}
	if o.LegacyTenant != "" && !seen[o.LegacyTenant] {
		return fmt.Errorf("state: legacy_tenant %q is not one of tenants", o.LegacyTenant)
	}
	for _, t := range o.Adopt {
		if !seen[t] {
			return fmt.Errorf("state: cannot adopt %q: it is not one of tenants", t)
		}
	}
	return nil
}

// Layout is an open shard state: the shard file and a store per declared tenant. It holds
// the directory's lock until Close.
type Layout struct {
	Dir     string
	Shard   *Shard
	Tenants map[string]*Store // every declared tenant, served
	// Notes say what opening did, in words, for the log.
	Notes []string
	lock  *os.File
}

// Close releases the directory's lock.
func (l *Layout) Close() error {
	if l.lock == nil {
		return nil
	}
	err := l.lock.Close()
	l.lock = nil
	return err
}

// Open opens a shard's state for writing, under the directory's lock:
//
//   - shard.json present: checked against the identity; every declared tenant on its roster
//     must have its file (a missing one was lost: refused, never begun afresh); a declared
//     tenant not on the roster is new: its file is created, then it joins the roster.
//   - a pre-tenant state.json present: merged into the tenant that owns it (LegacyTenant, or
//     the tenant of an earlier merge), marks only forward; the tenant file is written first,
//     shard.json second (its presence means "done"), then state.json is renamed away. A crash
//     at any point re-runs to the same result.
//   - nothing: ErrNotFound, unless Create (a brand-new shard).
func Open(o Options) (*Layout, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	if o.Create {
		if err := os.MkdirAll(o.Dir, 0o750); err != nil {
			return nil, fmt.Errorf("create state dir %s: %w", o.Dir, err)
		}
	}
	lock, err := lockDir(o.Dir)
	if err != nil {
		return nil, err
	}
	p, err := makePlan(o)
	if err == nil {
		var l *Layout
		if l, err = p.apply(); err == nil {
			l.lock = lock
			return l, nil
		}
	}
	lock.Close()
	return nil, err
}

// View is what the service would serve, read without changing anything.
type View struct {
	Dir             string
	Identity        Identity
	SnowflakeMarkMs int64
	Roster          []RosterEntry
	MigratedFrom    *Migration
	Tenants         map[string][]Record // every declared tenant, as the service would serve it
	// Notes say what the service's next start would do, in words.
	Notes []string
}

// Inspect reads a shard's state the way Open would, without writing or locking anything: what
// the service's next start would do (Notes), and the error it would refuse with.
func Inspect(o Options) (*View, error) {
	if err := o.check(); err != nil {
		return nil, err
	}
	p, err := makePlan(o)
	if err != nil {
		return nil, err
	}
	v := &View{
		Dir:             o.Dir,
		Identity:        p.shard.Identity,
		SnowflakeMarkMs: p.shard.SnowflakeMarkMs,
		Roster:          append([]RosterEntry(nil), p.shard.Tenants...),
		MigratedFrom:    p.shard.MigratedFrom,
		Tenants:         map[string][]Record{},
		Notes:           p.notes,
	}
	for _, t := range o.Tenants {
		v.Tenants[t] = append([]Record(nil), p.tenants[t].Records...)
	}
	return v, nil
}

// plan is what Open will write, worked out without writing anything.
type plan struct {
	o            Options
	shard        *shardFile             // shard.json as it will be
	writeShard   bool                   // shard.json must be written
	tenants      map[string]*tenantFile // every declared tenant's file as it will be
	writeTenants []string               // tenant files to write, in order
	legacy       *legacyFile            // a merged pre-tenant file, renamed away after the writes
	notes        []string
}

func makePlan(o Options) (*plan, error) {
	if fi, err := os.Stat(o.Dir); err == nil && !fi.IsDir() {
		return nil, fmt.Errorf("state_dir %s is not a directory", o.Dir)
	}
	p := &plan{o: o, tenants: map[string]*tenantFile{}}
	shardPath := filepath.Join(o.Dir, ShardFile)
	legacyPath := filepath.Join(o.Dir, LegacyFile)
	now := time.Now().UTC().Format(time.RFC3339)

	sf, err := readShardFile(shardPath, o.Identity)
	if err != nil {
		return nil, err
	}
	lf, err := readLegacy(legacyPath, o.Identity)
	if err != nil {
		return nil, err
	}
	switch {
	case sf != nil:
		p.shard = sf
	case lf != nil:
		p.shard, p.writeShard = newShardFile(o.Identity), true
	default:
		onDisk, err := tenantFilesOnDisk(o.Dir)
		if err != nil {
			return nil, err
		}
		if len(onDisk) > 0 {
			return nil, fmt.Errorf("%s is missing but %s holds tenant files (%s): this shard's state is incomplete — "+
				"restore %s from backup (a fresh one would lose the Snowflake mark and the tenant roster)",
				shardPath, filepath.Join(o.Dir, TenantsDir), strings.Join(onDisk, ", "), ShardFile)
		}
		if !o.Create {
			return nil, fmt.Errorf("%w: %s holds neither %s nor a pre-tenant %s", ErrNotFound, o.Dir, ShardFile, LegacyFile)
		}
		p.shard, p.writeShard = newShardFile(o.Identity), true
		p.notes = append(p.notes, fmt.Sprintf("create the state of a brand-new shard %d/%d", o.Identity.ShardID, o.Identity.TotalShards))
	}

	for _, t := range o.Tenants {
		tf, err := readTenantFile(tenantPath(o.Dir, t), t, o.Identity)
		if err != nil {
			return nil, err
		}
		if tf != nil {
			p.tenants[t] = tf
		}
	}

	if lf != nil {
		if err := p.planMerge(lf, sf != nil, now); err != nil {
			return nil, err
		}
	}

	adopt := map[string]bool{}
	for _, t := range o.Adopt {
		adopt[t] = true
	}
	for _, t := range o.Tenants {
		on, tf := p.shard.onRoster(t), p.tenants[t]
		switch {
		case on && tf == nil:
			return nil, fmt.Errorf("tenant %s is on this shard's roster but its state file %s is missing: it was lost — "+
				"restore it from backup (a fresh file would re-issue its IDs)", t, tenantPath(o.Dir, t))
		case on:
		case tf == nil:
			p.tenants[t] = newTenantFile(t, o.Identity)
			p.writeTenants = append(p.writeTenants, t)
			p.addToRoster(t, now)
			p.notes = append(p.notes, fmt.Sprintf("create tenant %s: declared, not on the roster yet", t))
		case len(tf.Records) == 0 || adopt[t]:
			p.addToRoster(t, now)
			p.notes = append(p.notes, fmt.Sprintf("put tenant %s's existing file (%d entities) on the roster", t, len(tf.Records)))
		default:
			return nil, fmt.Errorf("tenant %s has a state file with %d entities (%s) but is not on this shard's roster: it was not "+
				"created here. If %s was restored from a backup older than that file and the file is this shard's own, put it on "+
				"the roster with: bucket-next-cli -config <file> adopt-tenant %s", t, len(tf.Records), tenantPath(o.Dir, t), ShardFile, t)
		}
	}

	declared := map[string]bool{}
	for _, t := range o.Tenants {
		declared[t] = true
	}
	for _, e := range p.shard.Tenants {
		if !declared[e.Tenant] {
			p.notes = append(p.notes, fmt.Sprintf("tenant %s is on the roster but not in tenants: its file is kept and not served", e.Tenant))
		}
	}
	onDisk, err := tenantFilesOnDisk(o.Dir)
	if err != nil {
		return nil, err
	}
	for _, t := range onDisk {
		if !declared[t] && !p.shard.onRoster(t) {
			p.notes = append(p.notes, fmt.Sprintf("tenant file %s is neither declared nor on the roster: kept, not served", tenantPath(o.Dir, t)))
		}
	}
	return p, nil
}

// planMerge plans carrying a pre-tenant state.json into the tenant that owns it.
func (p *plan) planMerge(lf *legacyFile, besideShard bool, now string) error {
	o := p.o
	if o.NoMigrate {
		return fmt.Errorf("%s is a pre-tenant state file (format %d): the service carries it over at its next start — "+
			"refusing to change the state offline before that (an older binary may still be writing it)", lf.Path, lf.Format)
	}
	owner := o.LegacyTenant
	if owner == "" && p.shard.MigratedFrom != nil {
		owner = p.shard.MigratedFrom.Tenant
	}
	if owner == "" {
		return fmt.Errorf("%s is a pre-tenant state file (format %d, %d entities) and no tenant owns its counters: name that "+
			"tenant in legacy_tenant (one of tenants) — the service never guesses it", lf.Path, lf.Format, len(lf.Records))
	}
	if !contains(o.Tenants, owner) {
		return fmt.Errorf("the counters of %s belong to tenant %s, which is not in tenants", lf.Path, owner)
	}
	tf := p.tenants[owner]
	if tf == nil {
		tf = newTenantFile(owner, o.Identity)
		p.tenants[owner] = tf
	}
	changed, err := mergeLegacy(tf, lf, o.Identity.ShardID)
	if err != nil {
		return err
	}
	p.writeTenants = append(p.writeTenants, owner)
	if lf.SfMark > p.shard.SnowflakeMarkMs {
		p.shard.SnowflakeMarkMs = lf.SfMark
	}
	p.shard.MigratedFrom = &Migration{File: LegacyFile, Format: lf.Format, Tenant: owner, At: now}
	p.writeShard = true
	p.addToRoster(owner, now)
	p.legacy = lf
	switch {
	case besideShard && changed == 0:
		p.notes = append(p.notes, fmt.Sprintf("a pre-tenant %s sat beside %s with no mark ahead of tenant %s's (a carry-over "+
			"interrupted before its last step): nothing moves, it is renamed away", LegacyFile, ShardFile, owner))
	case besideShard:
		p.notes = append(p.notes, fmt.Sprintf("WARNING: a pre-tenant %s sat beside %s with %d of its %d marks ahead of tenant %s's "+
			"(an older binary ran here after the carry-over): merge it into %s, marks only forward", LegacyFile, ShardFile,
			changed, len(lf.Records), owner, owner))
	default:
		p.notes = append(p.notes, fmt.Sprintf("carry the pre-tenant %s (format %d, %d entities) over into tenant %s, every mark kept",
			LegacyFile, lf.Format, len(lf.Records), owner))
	}
	return nil
}

func (p *plan) addToRoster(t, now string) {
	if p.shard.onRoster(t) {
		return
	}
	p.shard.Tenants = append(p.shard.Tenants, RosterEntry{Tenant: t, AddedAt: now})
	p.writeShard = true
}

// apply writes the plan: tenant files first, shard.json second, then the merged
// state.json is renamed away. shard.json listing a tenant means its file is durable.
func (p *plan) apply() (*Layout, error) {
	o := p.o
	tdir := filepath.Join(o.Dir, TenantsDir)
	if _, err := os.Stat(tdir); os.IsNotExist(err) {
		if err := os.Mkdir(tdir, 0o750); err != nil {
			return nil, fmt.Errorf("create %s: %w", tdir, err)
		}
		if err := syncDir(o.Dir); err != nil {
			return nil, err
		}
	}
	for _, t := range p.writeTenants {
		if err := writeJSONDurable(tenantPath(o.Dir, t), p.tenants[t]); err != nil {
			return nil, err
		}
	}
	if p.writeShard {
		if err := writeJSONDurable(filepath.Join(o.Dir, ShardFile), p.shard); err != nil {
			return nil, err
		}
	}
	notes := append([]string(nil), p.notes...)
	if p.legacy != nil {
		dst, err := retireLegacy(o.Dir, p.legacy.Format)
		if err != nil {
			return nil, err
		}
		notes = append(notes, fmt.Sprintf("renamed the merged %s to %s: kept, never read again", LegacyFile, filepath.Base(dst)))
	}
	l := &Layout{
		Dir:     o.Dir,
		Shard:   newShard(filepath.Join(o.Dir, ShardFile), p.shard),
		Tenants: make(map[string]*Store, len(o.Tenants)),
		Notes:   notes,
	}
	for _, t := range o.Tenants {
		l.Tenants[t] = newStore(tenantPath(o.Dir, t), p.tenants[t])
	}
	return l, nil
}

// tenantFilesOnDisk lists the tenants that have a file in dir/tenants, sorted.
func tenantFilesOnDisk(dir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, TenantsDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, TenantsDir), err)
	}
	var out []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && !e.IsDir() && names.ValidTenant(name) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
