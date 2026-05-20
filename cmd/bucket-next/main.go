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

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/server"
	"github.com/telcobright/bucket-next/internal/state"
)

func main() {
	var cfgPath string
	flag.StringVar(&cfgPath, "config", "", "path to YAML config file (required)")
	flag.Parse()

	// Positional fallback: `bucket-next path/to/config.yaml`
	if cfgPath == "" && flag.NArg() == 1 {
		cfgPath = flag.Arg(0)
	}
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "usage: bucket-next -config <path/to/config.yaml>")
		os.Exit(2)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	st, err := state.Open(cfg.StatePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "state: %v\n", err)
		os.Exit(1)
	}

	fg, err := forge.New(cfg.ShardID, cfg.SnowflakeEpochMs, cfg.ClockDriftToleranceMs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v\n", err)
		os.Exit(1)
	}

	alloc := allocator.New(allocator.Config{
		ShardID:     cfg.ShardID,
		TotalShards: cfg.TotalShards,
		SegmentSize: cfg.SegmentSize,
		Watermark:   cfg.SegmentRefillWatermark,
		Store:       st,
	})

	srv := server.New(cfg, st, fg, alloc)

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
		alloc.Close() // wait for in-flight segment refills
		close(idleConnsClosed)
	}()

	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
	<-idleConnsClosed
	log.Println("bye")
}
