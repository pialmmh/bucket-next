package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/shardmath"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// CLI for offline maintenance of a shard's state. Reads the same YAML config as the service
// and refuses state that belongs to another shard. The commands that change state take the
// state directory's lock, so they are refused while the service runs; the ones that only
// read never write anything (not even on a pre-tenant directory the service has not carried
// over yet).

// exit codes of check
const (
	exitRefused = 1
	exitNoState = 3
)

func main() {
	var cfgPath, tenantName string
	flag.StringVar(&cfgPath, "config", "", "path to YAML config file (required)")
	flag.StringVar(&tenantName, "tenant", "", "the tenant a per-tenant command acts on (one of the config's tenants)")
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "help" {
		usage()
		return
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "error: -config <path> is required")
		os.Exit(2)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		die("config: %v", err)
	}
	o := state.Options{Dir: cfg.StateDir, Identity: identityOf(cfg), Tenants: cfg.Tenants, LegacyTenant: cfg.LegacyTenant}
	layout := shardmath.New(cfg.ShardID, cfg.TotalShards)

	switch cmd {
	case "check":
		cmdCheck(o)
		return
	case "info":
		cmdInfo(cfg, inspect(o, cfgPath))
		return
	case "tenants":
		cmdTenants(cfg, inspect(o, cfgPath))
		return
	case "init-state":
		cmdInitState(cfg, o)
		return
	case "adopt-tenant":
		if len(rest) != 1 {
			die("usage: adopt-tenant <tenant>")
		}
		o.Adopt = []string{rest[0]}
		l := open(o, cfgPath)
		defer l.Close()
		fmt.Printf("OK tenant %s is on the roster of shard %d/%d\n", rest[0], cfg.ShardID, cfg.TotalShards)
		return
	}

	// every other command acts on one tenant
	if tenantName == "" {
		die("%s acts on one tenant: name it with -tenant <tenant> (one of %s)", cmd, strings.Join(cfg.Tenants, ", "))
	}
	if !contains(cfg.Tenants, tenantName) {
		die("tenant %q is not one of the config's tenants (%s)", tenantName, strings.Join(cfg.Tenants, ", "))
	}
	switch cmd {
	case "list":
		cmdList(cfg, layout, tenantName, inspect(o, cfgPath).Tenants[tenantName])
		return
	case "status":
		cmdStatus(cfg, layout, tenantName, inspect(o, cfgPath).Tenants[tenantName], rest)
		return
	}

	l := open(o, cfgPath)
	defer l.Close()
	st := l.Tenants[tenantName]
	switch cmd {
	case "init":
		cmdInit(layout, st, rest)
	case "reset":
		cmdReset(cfg, layout, st, rest)
	case "delete":
		cmdDelete(st, rest)
	case "clear":
		cmdClear(st, rest)
	case "rename":
		cmdRename(cfg, st, rest)
	case "export-v2":
		cmdExportV2(l, tenantName)
	default:
		l.Close()
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}
}

func identityOf(cfg *config.Config) state.Identity {
	key, err := cfg.UUIDKeyBytes()
	if err != nil {
		die("config: %v", err)
	}
	if key == nil {
		key = uuidgen.DefaultKey()
	}
	return state.Identity{
		ShardID:            cfg.ShardID,
		TotalShards:        cfg.TotalShards,
		UUIDKeyFingerprint: uuidgen.Fingerprint(key),
		SnowflakeEpochMs:   cfg.SnowflakeEpochMs,
	}
}

// inspect reads the state as the service would serve it, writing nothing.
func inspect(o state.Options, cfgPath string) *state.View {
	v, err := state.Inspect(o)
	if errors.Is(err, state.ErrNotFound) {
		die("%v\nFor a brand-new shard run: bucket-next-cli -config %s init-state", err, cfgPath)
	}
	if err != nil {
		die("state: %v", err)
	}
	return v
}

// open opens the state for a change, under the directory's lock. A pre-tenant state.json is
// refused: the service carries it over at its next start, and the old binary may still be
// writing it.
func open(o state.Options, cfgPath string) *state.Layout {
	o.NoMigrate = true
	l, err := state.Open(o)
	if errors.Is(err, state.ErrNotFound) {
		die("%v\nFor a brand-new shard run: bucket-next-cli -config %s init-state", err, cfgPath)
	}
	if err != nil {
		die("state: %v", err)
	}
	return l
}

// ---------- shard commands ----------

// cmdCheck says what the service's next start would do, changing nothing. Exit 0: it would
// start; 1: it would refuse (the reason is printed); 3: there is no state yet.
func cmdCheck(o state.Options) {
	v, err := state.Inspect(o)
	if errors.Is(err, state.ErrNotFound) {
		fmt.Printf("NO STATE: %v\n", err)
		os.Exit(exitNoState)
	}
	if err != nil {
		fmt.Printf("REFUSE: the service would not start: %v\n", err)
		os.Exit(exitRefused)
	}
	if len(v.Notes) == 0 {
		fmt.Println("OK: the next start opens the state as it is")
	}
	for _, n := range v.Notes {
		fmt.Printf("OK: the next start will %s\n", n)
	}
	for _, t := range o.Tenants {
		fmt.Printf("  tenant %-16s %d entities\n", t, len(v.Tenants[t]))
	}
}

func cmdInitState(cfg *config.Config, o state.Options) {
	if _, err := state.Inspect(o); err == nil {
		die("the state of shard %d/%d already exists in %s — init-state is for a brand-new shard only", cfg.ShardID, cfg.TotalShards, cfg.StateDir)
	} else if !errors.Is(err, state.ErrNotFound) {
		die("state: %v", err)
	}
	o.Create = true
	o.NoMigrate = true
	l, err := state.Open(o)
	if err != nil {
		die("%v", err)
	}
	defer l.Close()
	fmt.Printf("OK created the state of shard %d/%d in %s for tenants %s\n", cfg.ShardID, cfg.TotalShards, cfg.StateDir, strings.Join(cfg.Tenants, ", "))
}

func cmdInfo(cfg *config.Config, v *state.View) {
	id := v.Identity
	fmt.Printf("Shard:            %d/%d\n", id.ShardID, id.TotalShards)
	fmt.Printf("State dir:        %s\n", cfg.StateDir)
	fmt.Printf("uuid key:         fingerprint %s\n", id.UUIDKeyFingerprint)
	fmt.Printf("Snowflake epoch:  %d\n", id.SnowflakeEpochMs)
	fmt.Printf("Snowflake mark:   %d\n", v.SnowflakeMarkMs)
	if m := v.MigratedFrom; m != nil {
		fmt.Printf("Carried over:     %s (format %d) into tenant %s at %s\n", m.File, m.Format, m.Tenant, m.At)
	}
	fmt.Printf("legacy_tenant:    %s\n", orNone(cfg.LegacyTenant))
	for _, t := range cfg.Tenants {
		counts := map[datatype.DataType]int{}
		for _, r := range v.Tenants[t] {
			counts[r.DataType]++
		}
		var parts []string
		for _, dt := range datatype.All {
			if counts[dt] > 0 {
				parts = append(parts, fmt.Sprintf("%s %d", dt, counts[dt]))
			}
		}
		fmt.Printf("Tenant %-10s %d entities %s\n", t+":", len(v.Tenants[t]), strings.Join(parts, ", "))
	}
	for _, n := range v.Notes {
		fmt.Printf("Next start will:  %s\n", n)
	}
}

func cmdTenants(cfg *config.Config, v *state.View) {
	onRoster := map[string]string{}
	for _, e := range v.Roster {
		onRoster[e.Tenant] = e.AddedAt
	}
	fmt.Printf("%-16s %-8s %-8s %-22s %s\n", "TENANT", "SERVED", "ENTITIES", "ON ROSTER SINCE", "FILE")
	for _, t := range cfg.Tenants {
		since := onRoster[t]
		if since == "" {
			since = "(joins at next start)"
		}
		fmt.Printf("%-16s %-8s %-8d %-22s %s/%s/%s.json\n", t, "yes", len(v.Tenants[t]), since, cfg.StateDir, state.TenantsDir, t)
	}
	for _, e := range v.Roster {
		if !contains(cfg.Tenants, e.Tenant) {
			fmt.Printf("%-16s %-8s %-8s %-22s %s/%s/%s.json (kept, not declared)\n", e.Tenant, "no", "-", e.AddedAt, cfg.StateDir, state.TenantsDir, e.Tenant)
		}
	}
}

// ---------- tenant commands ----------

// The service may have issued any value below the stored mark (it reserves ahead), so the
// CLI treats the mark as "next value" and never moves below it without --force.

func cmdList(cfg *config.Config, layout shardmath.Layout, tenant string, records []state.Record) {
	if len(records) == 0 {
		fmt.Printf("No entities registered for tenant %s.\n", tenant)
		return
	}
	fmt.Printf("Tenant %s, registered entities (shard %d/%d):\n", tenant, cfg.ShardID, cfg.TotalShards)
	for _, r := range records {
		fmt.Printf("\nEntity: %s\n  Type:        %s\n  Mark:        %d\n", r.EntityName, r.DataType, r.CurrentIteration)
		if r.DataType.IsNumeric() {
			fmt.Printf("  Next value:  %d\n", layout.Value(r.CurrentIteration))
		}
	}
}

func cmdStatus(cfg *config.Config, layout shardmath.Layout, tenant string, records []state.Record, args []string) {
	if len(args) != 1 {
		die("usage: -tenant <tenant> status <entity>")
	}
	for _, r := range records {
		if r.EntityName != args[0] {
			continue
		}
		fmt.Printf("Tenant:      %s\nEntity:      %s\nType:        %s\nShard:       %d/%d\nMark:        %d\n",
			tenant, r.EntityName, r.DataType, cfg.ShardID, cfg.TotalShards, r.CurrentIteration)
		if r.DataType.IsNumeric() {
			fmt.Printf("Next value:  %d\nPattern:     %d, %d, %d, ...\n",
				layout.Value(r.CurrentIteration),
				cfg.ShardID, cfg.ShardID+cfg.TotalShards, cfg.ShardID+2*cfg.TotalShards)
		}
		return
	}
	die("entity %q not found in tenant %s", args[0], tenant)
}

func cmdInit(layout shardmath.Layout, st *state.Store, args []string) {
	if len(args) < 2 || len(args) > 3 {
		die("usage: -tenant <tenant> init <entity> <type> [start-value]")
	}
	name := args[0]
	dt, err := datatype.Parse(args[1])
	if err != nil {
		die("%v", err)
	}
	var iter int64
	if len(args) == 3 {
		if !dt.IsNumeric() {
			die("start-value is only supported for int and long")
		}
		snapped := snapValue(layout, dt, args[2])
		iter = layout.Iter(snapped)
	}
	if _, created, err := st.Register(name, dt, iter); err != nil {
		die("write state: %v", err)
	} else if !created {
		die("entity %q already exists in tenant %s", name, st.Tenant())
	}
	fmt.Printf("OK initialised %q as %s for tenant %s on shard %d", name, dt, st.Tenant(), layout.ShardID)
	if dt.IsNumeric() {
		fmt.Printf("; first value will be %d", layout.Value(iter))
	}
	fmt.Println()
}

func cmdReset(cfg *config.Config, layout shardmath.Layout, st *state.Store, args []string) {
	force := hasFlag(&args, "--force")
	if len(args) != 2 {
		die("usage: -tenant <tenant> reset <entity> <value> [--force]")
	}
	name := args[0]
	r, ok := st.Get(name)
	if !ok {
		die("entity %q not found in tenant %s", name, st.Tenant())
	}
	if !r.DataType.IsNumeric() {
		die("reset only valid for int and long; %q is %s", name, r.DataType)
	}
	snapped := snapValue(layout, r.DataType, args[1])
	newIter := layout.Iter(snapped)
	if newIter < r.CurrentIteration && !force {
		die("refused: next value is %d and the service may have issued anything below it; "+
			"moving to %d could re-issue IDs. Add --force to do it anyway.",
			layout.Value(r.CurrentIteration), snapped)
	}
	if err := st.SetMark(name, newIter); err != nil {
		die("write state: %v", err)
	}
	fmt.Printf("OK %q of tenant %s on shard %d/%d: next value %d -> %d\n",
		name, st.Tenant(), cfg.ShardID, cfg.TotalShards, layout.Value(r.CurrentIteration), snapped)
}

func cmdDelete(st *state.Store, args []string) {
	force := hasFlag(&args, "--force")
	if len(args) != 1 {
		die("usage: -tenant <tenant> delete <entity> --force")
	}
	if _, ok := st.Get(args[0]); !ok {
		die("entity %q not found in tenant %s", args[0], st.Tenant())
	}
	if !force {
		die("refused: if %q is used again it restarts from its first value and re-issues IDs. "+
			"Add --force to delete anyway.", args[0])
	}
	if err := st.Delete(args[0]); err != nil {
		die("delete: %v", err)
	}
	fmt.Printf("OK deleted %q from tenant %s\n", args[0], st.Tenant())
}

func cmdClear(st *state.Store, args []string) {
	if !hasFlag(&args, "--force") || len(args) != 0 {
		die("usage: -tenant <tenant> clear --force   (every entity of the tenant restarts from its first value; IDs will be re-issued)")
	}
	n := len(st.All())
	if err := st.Clear(); err != nil {
		die("clear: %v", err)
	}
	fmt.Printf("OK cleared %d entities of tenant %s\n", n, st.Tenant())
}

// cmdRename carries an entity's counter to a new name. While the pre-tenant roads act on
// this tenant it is refused: a pre-tenant caller asking for the old name would register it
// afresh, and two counters would mint the same IDs.
func cmdRename(cfg *config.Config, st *state.Store, args []string) {
	if len(args) != 2 {
		die("usage: -tenant <tenant> rename <from> <to>")
	}
	if cfg.LegacyTenant == st.Tenant() {
		die("refused: legacy_tenant is %s, so the pre-tenant roads still act on it — a caller asking for %q there "+
			"would register it afresh and two counters would mint the same IDs. Switch those roads off first "+
			"(remove legacy_tenant), then rename.", st.Tenant(), args[0])
	}
	if err := st.Rename(args[0], args[1]); err != nil {
		die("rename: %v", err)
	}
	fmt.Printf("OK renamed %q to %q in tenant %s; its counter carries on\n", args[0], args[1], st.Tenant())
}

func cmdExportV2(l *state.Layout, tenant string) {
	path, err := l.ExportV2(tenant)
	if err != nil {
		die("export-v2: %v", err)
	}
	fmt.Printf("OK wrote %s (format 2) from tenant %s's current marks and retired %s.\n", path, tenant, state.ShardFile)
	fmt.Printf("The pre-tenant binary now starts with state_path: %s\n", path)
	fmt.Println("A later start of this binary carries that file over again (marks only move forward).")
}

// ---------- helpers ----------

// snapValue parses a user value for an int/long entity, checks its range and snaps it to
// this shard.
func snapValue(layout shardmath.Layout, dt datatype.DataType, s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		die("invalid value %q: %v", s, err)
	}
	if v < 1 || v > dt.MaxValue() {
		die("value must be between 1 and %d for %s", dt.MaxValue(), dt)
	}
	snapped := layout.SnapForward(v)
	if snapped < v || snapped > dt.MaxValue() {
		die("value %d has no slot for shard %d at or below %d", v, layout.ShardID, dt.MaxValue())
	}
	if snapped != v {
		fmt.Printf("snapped %d -> %d (shard %d residue)\n", v, snapped, layout.ShardID)
	}
	return snapped
}

// hasFlag removes flag from args and reports whether it was there.
func hasFlag(args *[]string, flag string) bool {
	found := false
	out := (*args)[:0]
	for _, a := range *args {
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	*args = out
	return found
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func orNone(s string) string {
	if s == "" {
		return "(none: the pre-tenant roads are refused)"
	}
	return s
}

func usage() {
	fmt.Println("bucket-next-cli — offline maintenance of a shard's state")
	fmt.Println()
	fmt.Println("Usage: bucket-next-cli -config <path> [-tenant <tenant>] <command> [args]")
	fmt.Println()
	fmt.Println("Shard commands:")
	fmt.Println("  check                             what the service's next start will do; changes nothing")
	fmt.Println("                                    (exit 0 it starts, 1 it would refuse, 3 no state yet)")
	fmt.Println("  info                              the shard: identity, Snowflake mark, carry-over, tenants")
	fmt.Println("  tenants                           the declared tenants and the roster")
	fmt.Println("  init-state                        create the state of a brand-new shard")
	fmt.Println("  adopt-tenant <tenant>             put a tenant's existing file on the roster")
	fmt.Println("                                    (after shard.json was restored from an older backup)")
	fmt.Println()
	fmt.Println("Tenant commands (-tenant <tenant>):")
	fmt.Println("  list                              list the tenant's entities")
	fmt.Println("  status <entity>                   show one entity")
	fmt.Println("  init <entity> <type> [start]      register an entity (start snaps to the shard)")
	fmt.Println("  reset <entity> <value> [--force]  move an int/long counter forward (--force to go back)")
	fmt.Println("  delete <entity> --force           remove one entity (its IDs restart if reused)")
	fmt.Println("  clear --force                     remove all the tenant's entities")
	fmt.Println("  rename <from> <to>                carry a counter to a new name (not uuid; not while legacy_tenant is this tenant)")
	fmt.Println("  export-v2                         the route back to a pre-tenant binary: state.json from the current marks")
	fmt.Println("  help                              show this help")
	fmt.Println()
	fmt.Println("The commands that change state take the state directory's lock: stop the service first.")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
