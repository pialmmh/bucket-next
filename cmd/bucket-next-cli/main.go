package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/shardmath"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// CLI for offline maintenance of the state store. Reads the same YAML config as the
// Service and refuses a state file that belongs to another shard. Stop the Service before
// running any command that changes state.

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "path to YAML config file (required)")
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
	identity := identityOf(cfg)
	layout := shardmath.New(cfg.ShardID, cfg.TotalShards)

	if cmd == "init-state" {
		if _, err := state.Create(cfg.StatePath, identity); err != nil {
			die("%v", err)
		}
		fmt.Printf("OK created %s for shard %d/%d\n", cfg.StatePath, cfg.ShardID, cfg.TotalShards)
		return
	}

	st, err := state.Open(cfg.StatePath, identity)
	if errors.Is(err, state.ErrNotFound) {
		die("%v\nFor a brand-new shard run: bucket-next-cli -config %s init-state", err, cfgPath)
	}
	if err != nil {
		die("state: %v", err)
	}

	switch cmd {
	case "list":
		cmdList(cfg, layout, st)
	case "status":
		cmdStatus(cfg, layout, st, rest)
	case "init":
		cmdInit(layout, st, rest)
	case "reset":
		cmdReset(cfg, layout, st, rest)
	case "delete":
		cmdDelete(st, rest)
	case "clear":
		cmdClear(st, rest)
	case "info":
		cmdInfo(cfg, st)
	default:
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

// ---------- commands ----------

// The service may have issued any value below the stored mark (it reserves ahead), so the
// CLI treats the mark as "next value" and never moves below it without --force.

func cmdList(cfg *config.Config, layout shardmath.Layout, st *state.Store) {
	records := st.All()
	if len(records) == 0 {
		fmt.Println("No entities registered.")
		return
	}
	fmt.Printf("Registered entities (shard %d/%d):\n", cfg.ShardID, cfg.TotalShards)
	for _, r := range records {
		fmt.Printf("\nEntity: %s\n  Type:        %s\n  Mark:        %d\n", r.EntityName, r.DataType, r.CurrentIteration)
		if r.DataType.IsNumeric() {
			fmt.Printf("  Next value:  %d\n", layout.Value(r.CurrentIteration))
		}
	}
}

func cmdStatus(cfg *config.Config, layout shardmath.Layout, st *state.Store, args []string) {
	if len(args) != 1 {
		die("usage: status <entity>")
	}
	r, ok := st.Get(args[0])
	if !ok {
		die("entity %q not found", args[0])
	}
	fmt.Printf("Entity:      %s\nType:        %s\nShard:       %d/%d\nMark:        %d\n",
		r.EntityName, r.DataType, cfg.ShardID, cfg.TotalShards, r.CurrentIteration)
	if r.DataType.IsNumeric() {
		fmt.Printf("Next value:  %d\nPattern:     %d, %d, %d, ...\n",
			layout.Value(r.CurrentIteration),
			cfg.ShardID, cfg.ShardID+cfg.TotalShards, cfg.ShardID+2*cfg.TotalShards)
	}
}

func cmdInit(layout shardmath.Layout, st *state.Store, args []string) {
	if len(args) < 2 || len(args) > 3 {
		die("usage: init <entity> <type> [start-value]")
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
		die("entity %q already exists", name)
	}
	fmt.Printf("OK initialised %q as %s on shard %d", name, dt, layout.ShardID)
	if dt.IsNumeric() {
		fmt.Printf("; first value will be %d", layout.Value(iter))
	}
	fmt.Println()
}

func cmdReset(cfg *config.Config, layout shardmath.Layout, st *state.Store, args []string) {
	force := hasFlag(&args, "--force")
	if len(args) != 2 {
		die("usage: reset <entity> <value> [--force]")
	}
	name := args[0]
	r, ok := st.Get(name)
	if !ok {
		die("entity %q not found", name)
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
	fmt.Printf("OK %q on shard %d/%d: next value %d -> %d\n",
		name, cfg.ShardID, cfg.TotalShards, layout.Value(r.CurrentIteration), snapped)
}

func cmdDelete(st *state.Store, args []string) {
	force := hasFlag(&args, "--force")
	if len(args) != 1 {
		die("usage: delete <entity> --force")
	}
	if _, ok := st.Get(args[0]); !ok {
		die("entity %q not found", args[0])
	}
	if !force {
		die("refused: if %q is used again it restarts from its first value and re-issues IDs. "+
			"Add --force to delete anyway.", args[0])
	}
	if err := st.Delete(args[0]); err != nil {
		die("delete: %v", err)
	}
	fmt.Printf("OK deleted %q\n", args[0])
}

func cmdClear(st *state.Store, args []string) {
	if !hasFlag(&args, "--force") || len(args) != 0 {
		die("usage: clear --force   (every entity restarts from its first value; IDs will be re-issued)")
	}
	n := len(st.All())
	if err := st.Clear(); err != nil {
		die("clear: %v", err)
	}
	fmt.Printf("OK cleared %d entities\n", n)
}

func cmdInfo(cfg *config.Config, st *state.Store) {
	records := st.All()
	id := st.Identity()
	fmt.Printf("Shard:            %d/%d\n", id.ShardID, id.TotalShards)
	fmt.Printf("State path:       %s\n", cfg.StatePath)
	fmt.Printf("uuid key:         fingerprint %s\n", id.UUIDKeyFingerprint)
	fmt.Printf("Snowflake epoch:  %d\n", id.SnowflakeEpochMs)
	fmt.Printf("Snowflake mark:   %d\n", st.SnowflakeMark())
	fmt.Printf("Entities:         %d\n", len(records))
	counts := map[datatype.DataType]int{}
	for _, r := range records {
		counts[r.DataType]++
	}
	for _, dt := range datatype.All {
		if counts[dt] > 0 {
			fmt.Printf("  %-10s %d\n", dt, counts[dt])
		}
	}
	if fi, err := os.Stat(cfg.StatePath); err == nil {
		fmt.Printf("File size:        %d bytes\n", fi.Size())
		fmt.Printf("Modified:         %s\n", fi.ModTime().Format("2006-01-02 15:04:05"))
	}
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

func usage() {
	fmt.Println("bucket-next-cli — offline maintenance for the state store")
	fmt.Println()
	fmt.Println("Usage: bucket-next-cli -config <path> <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  init-state                        create the state file for a brand-new shard")
	fmt.Println("  list                              list all entities")
	fmt.Println("  status <entity>                   show one entity")
	fmt.Println("  init <entity> <type> [start]      register an entity (start snaps to the shard)")
	fmt.Println("  reset <entity> <value> [--force]  move an int/long counter forward (--force to go back)")
	fmt.Println("  delete <entity> --force           remove one entity (its IDs restart if reused)")
	fmt.Println("  clear --force                     remove all entities")
	fmt.Println("  info                              system info")
	fmt.Println("  help                              show this help")
	fmt.Println()
	fmt.Println("Stop the service before init, reset, delete or clear.")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
