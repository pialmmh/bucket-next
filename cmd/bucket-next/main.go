package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/server"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

func main() {
	var cfgPath string
	var initState bool
	flag.StringVar(&cfgPath, "config", "", "path to YAML config file (required)")
	flag.BoolVar(&initState, "init", false, "create the shard's state when its directory holds none (first start of a brand-new shard only)")
	flag.Parse()

	// Positional fallback: `bucket-next path/to/config.yaml`
	if cfgPath == "" && flag.NArg() == 1 {
		cfgPath = flag.Arg(0)
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "usage: bucket-next -config <path/to/config.yaml> [-init]")
		os.Exit(2)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fail("config: %v", err)
	}

	key, err := cfg.UUIDKeyBytes()
	if err != nil {
		fail("config: %v", err)
	}
	if key == nil {
		key = uuidgen.DefaultKey()
		log.Printf("WARNING: uuid_key is not set — uuid IDs are unique but predictable to anyone with the source; " +
			"set the same uuid_key on every shard before issuing uuid IDs in production")
	}
	codec, err := uuidgen.New(key)
	if err != nil {
		fail("uuid key: %v", err)
	}

	identity := state.Identity{
		ShardID:            cfg.ShardID,
		TotalShards:        cfg.TotalShards,
		UUIDKeyFingerprint: codec.Fingerprint(),
		SnowflakeEpochMs:   cfg.SnowflakeEpochMs,
	}
	lay, err := state.Open(state.Options{
		Dir:          cfg.StateDir,
		Identity:     identity,
		Tenants:      cfg.Tenants,
		LegacyTenant: cfg.LegacyTenant,
		Create:       initState,
	})
	if errors.Is(err, state.ErrNotFound) {
		fail("%v\nRefusing to start: starting from zero would re-issue IDs.\n"+
			"If this is a brand-new shard, start it once with -init (or run: bucket-next-cli -config %s init-state).\n"+
			"If the state was lost, restore it from backup instead.", err, cfgPath)
	}
	if err != nil {
		fail("state: %v", err)
	}
	for _, note := range lay.Notes {
		log.Printf("state %s: %s", cfg.StateDir, note)
	}

	fg, err := forge.New(cfg.ShardID, cfg.SnowflakeEpochMs, cfg.ClockDriftToleranceMs)
	if err != nil {
		fail("forge: %v", err)
	}
	if err := fg.EnableMark(lay.Shard.SnowflakeMark(), lay.Shard.RaiseSnowflakeMark); err != nil {
		fail("forge: %v", err)
	}

	srv := server.New(cfg, lay, fg, codec)

	// Graceful shutdown plumbing.
	idleConnsClosed := make(chan struct{})
	go func() {
		sigs := make(chan os.Signal, 1)
		signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
		s := <-sigs
		log.Printf("received %s, shutting down...", s)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		srv.Close() // wait for every tenant's in-flight segment refills
		lay.Close() // release the state directory's lock
		close(idleConnsClosed)
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
	<-idleConnsClosed
	log.Println("bye")
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
