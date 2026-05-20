package main

import (
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/state"
)

// CLI for offline maintenance of the State store. Reads the same YAML config as the
// Service. Operator must stop the Service before running mutations.

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "path to YAML config file (required for shard-aware commands)")
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
	st, err := state.Open(cfg.StatePath)
	if err != nil {
		die("state: %v", err)
	}

	switch cmd {
	case "list":
		cmdList(cfg, st)
	case "status":
		cmdStatus(cfg, st, rest)
	case "init":
		cmdInit(cfg, st, rest)
	case "reset":
		cmdReset(cfg, st, rest)
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

// ---------- shard math (duplicates allocator helpers; the CLI does not run an Allocator) ----------

func computeValue(cfg *config.Config, iter int64) int64 {
	return int64(cfg.ShardID) + iter*int64(cfg.TotalShards)
}

func iterFromValue(cfg *config.Config, v int64) int64 {
	return (v - int64(cfg.ShardID)) / int64(cfg.TotalShards)
}

func snapForward(cfg *config.Config, v int64) int64 {
	N := int64(cfg.TotalShards)
	rem := ((v % N) + N) % N
	target := int64(cfg.ShardID) % N
	if rem == target {
		return v
	}
	diff := target - rem
	if diff < 0 {
		diff += N
	}
	return v + diff
}

// ---------- commands ----------

func cmdList(cfg *config.Config, st *state.Store) {
	records := st.All()
	if len(records) == 0 {
		fmt.Println("No entities registered.")
		return
	}
	fmt.Printf("Registered Entities (shard %d/%d):\n", cfg.ShardID, cfg.TotalShards)
	fmt.Println("================================")
	for _, r := range records {
		fmt.Printf("\nEntity: %s\n  Type:        %s\n  Iteration:   %d\n",
			r.EntityName, r.DataType, r.CurrentIteration)
		if r.DataType.IsNumeric() {
			fmt.Printf("  Next value:  %d\n", computeValue(cfg, r.CurrentIteration))
		}
	}
}

func cmdStatus(cfg *config.Config, st *state.Store, args []string) {
	if len(args) != 1 {
		die("usage: status <entity>")
	}
	r, ok := st.Get(args[0])
	if !ok {
		die("entity %q not found", args[0])
	}
	fmt.Printf("Entity:      %s\nType:        %s\nShard:       %d/%d\nIteration:   %d\n",
		r.EntityName, r.DataType, cfg.ShardID, cfg.TotalShards, r.CurrentIteration)
	if r.DataType.IsNumeric() {
		fmt.Printf("Next value:  %d\nPattern:     %d, %d, %d, ...\n",
			computeValue(cfg, r.CurrentIteration),
			cfg.ShardID, cfg.ShardID+cfg.TotalShards, cfg.ShardID+2*cfg.TotalShards)
	}
}

func cmdInit(cfg *config.Config, st *state.Store, args []string) {
	if len(args) < 2 || len(args) > 3 {
		die("usage: init <entity> <type> [start-value]")
	}
	name := args[0]
	dt, err := datatype.Parse(args[1])
	if err != nil {
		die("%v", err)
	}
	if _, ok := st.Get(name); ok {
		die("entity %q already exists", name)
	}

	var iter int64
	if len(args) == 3 {
		if !dt.IsNumeric() {
			die("start-value only valid for numeric types")
		}
		requested, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil {
			die("invalid start-value: %v", err)
		}
		snapped := snapForward(cfg, requested)
		if snapped != requested {
			fmt.Printf("snapped start-value %d -> %d (shard %d residue)\n",
				requested, snapped, cfg.ShardID)
		}
		iter = iterFromValue(cfg, snapped)
	}
	if err := st.Put(state.Record{
		EntityName:       name,
		DataType:         dt,
		CurrentIteration: iter,
		ShardID:          cfg.ShardID,
	}); err != nil {
		die("write state: %v", err)
	}
	fmt.Printf("OK initialised %q as %s", name, dt)
	if dt.IsNumeric() {
		fmt.Printf("; first value will be %d", computeValue(cfg, iter))
	}
	fmt.Println()
}

func cmdReset(cfg *config.Config, st *state.Store, args []string) {
	if len(args) != 2 {
		die("usage: reset <entity> <value>")
	}
	name := args[0]
	r, ok := st.Get(name)
	if !ok {
		die("entity %q not found", name)
	}
	if !r.DataType.IsNumeric() {
		die("reset only valid for int and long; %q is %s", name, r.DataType)
	}
	requested, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		die("invalid value: %v", err)
	}
	snapped := snapForward(cfg, requested)
	if snapped != requested {
		fmt.Printf("snapped %d -> %d (shard %d residue)\n", requested, snapped, cfg.ShardID)
	}
	prev := r.CurrentIteration
	r.CurrentIteration = iterFromValue(cfg, snapped)
	if err := st.Put(*r); err != nil {
		die("write state: %v", err)
	}
	fmt.Printf("OK %q reset: iteration %d -> %d; next value will be %d\n",
		name, prev, r.CurrentIteration, computeValue(cfg, r.CurrentIteration))
}

func cmdDelete(st *state.Store, args []string) {
	if len(args) != 1 {
		die("usage: delete <entity>")
	}
	if _, ok := st.Get(args[0]); !ok {
		die("entity %q not found", args[0])
	}
	if err := st.Delete(args[0]); err != nil {
		die("delete: %v", err)
	}
	fmt.Printf("OK deleted %q\n", args[0])
}

func cmdClear(st *state.Store, args []string) {
	if len(args) != 1 || args[0] != "--force" {
		die("usage: clear --force   (refuses without --force)")
	}
	n := len(st.All())
	if err := st.Clear(); err != nil {
		die("clear: %v", err)
	}
	fmt.Printf("OK cleared %d entities\n", n)
}

func cmdInfo(cfg *config.Config, st *state.Store) {
	records := st.All()
	fmt.Printf("Shard:      %d/%d\n", cfg.ShardID, cfg.TotalShards)
	fmt.Printf("State path: %s\n", cfg.StatePath)
	fmt.Printf("Entities:   %d\n", len(records))
	counts := map[datatype.DataType]int{}
	for _, r := range records {
		counts[r.DataType]++
	}
	if len(counts) > 0 {
		fmt.Println("By type:")
		for _, dt := range datatype.All {
			if counts[dt] > 0 {
				fmt.Printf("  %-10s %d\n", dt, counts[dt])
			}
		}
	}
	if fi, err := os.Stat(cfg.StatePath); err == nil {
		fmt.Printf("File size:  %d bytes\n", fi.Size())
		fmt.Printf("Modified:   %s\n", fi.ModTime().Format("2006-01-02 15:04:05"))
	}
}

// ---------- helpers ----------

func usage() {
	fmt.Println("bucket-next-cli — offline maintenance for the State store")
	fmt.Println()
	fmt.Println("Usage: bucket-next-cli -config <path> <command> [args]")
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  list                              list all entities")
	fmt.Println("  status <entity>                   show one entity")
	fmt.Println("  init <entity> <type> [start]      register an entity (snaps start to shard)")
	fmt.Println("  reset <entity> <value>            move numeric counter (snaps to shard)")
	fmt.Println("  delete <entity>                   remove one entity")
	fmt.Println("  clear --force                     remove all entities")
	fmt.Println("  info                              system info")
	fmt.Println("  help                              show this help")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
