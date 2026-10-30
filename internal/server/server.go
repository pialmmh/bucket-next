package server

import (
	"context"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/shardmath"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// tenant is one served tenant: its entities on this shard and their in-RAM reservations.
type tenant struct {
	name  string
	store *state.Store
	alloc *allocator.Allocator
}

type Server struct {
	cfg       *config.Config
	shard     *state.Shard
	tenants   map[string]*tenant
	served    []string // the served tenants, sorted
	legacy    *tenant  // the tenant the pre-tenant roads act on; nil = those roads are refused
	layout    shardmath.Layout
	forge     *forge.Forge
	codec     *uuidgen.Codec
	startedAt time.Time
	mux       *http.ServeMux
	httpsrv   *http.Server
}

// New serves every tenant of the open state l. The forge is shared by every tenant; each
// tenant gets its own allocator over its own file.
func New(cfg *config.Config, l *state.Layout, f *forge.Forge, codec *uuidgen.Codec) *Server {
	s := &Server{
		cfg:       cfg,
		shard:     l.Shard,
		tenants:   make(map[string]*tenant, len(l.Tenants)),
		layout:    shardmath.New(cfg.ShardID, cfg.TotalShards),
		forge:     f,
		codec:     codec,
		startedAt: time.Now(),
		mux:       http.NewServeMux(),
	}
	for name, st := range l.Tenants {
		s.tenants[name] = &tenant{name: name, store: st, alloc: allocator.New(allocator.Config{
			ShardID:     cfg.ShardID,
			TotalShards: cfg.TotalShards,
			SegmentSize: cfg.SegmentSize,
			Watermark:   cfg.SegmentRefillWatermark,
			Store:       st,
		})}
		s.served = append(s.served, name)
	}
	sort.Strings(s.served)
	if cfg.LegacyTenant != "" {
		s.legacy = s.tenants[cfg.LegacyTenant]
	}
	s.routes()
	s.httpsrv = &http.Server{
		Addr:              cfg.ListenAddr(),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", methodOnly(http.MethodGet, s.handleHealth))
	s.mux.HandleFunc("/shard-info", methodOnly(http.MethodGet, s.handleShardInfo))

	// The pre-tenant roads: no tenant in the path, on legacy_tenant only.
	s.v1("/api/types", http.MethodGet, s.types)
	s.v1("/api/list", http.MethodGet, s.list)
	s.v1("/api/next-id/", http.MethodGet, s.nextID)
	s.v1("/api/next-batch/", http.MethodGet, s.nextBatch)
	s.v1("/api/init/", http.MethodPost, s.initEntity)
	s.v1("/api/reset/", http.MethodPut, s.reset)
	s.v1("/api/status/", http.MethodGet, s.status)
	s.v1("/api/segment-state/", http.MethodGet, s.segmentState)
	s.v1("/api/parse-snowflake/", http.MethodGet, s.parseSnowflake)
}

// v1 registers a pre-tenant road. It answers exactly as before tenancy, on legacy_tenant;
// with no legacy_tenant it refuses, naming the tenant-first road to call instead.
func (s *Server) v1(prefix, method string, h road) {
	s.mux.HandleFunc(prefix, methodOnly(method, func(w http.ResponseWriter, r *http.Request) {
		if s.legacy == nil {
			writeError(w, http.StatusBadRequest, "Tenant is required",
				"this installation serves tenants by name: call /api/v2/{tenant}/"+strings.TrimPrefix(r.URL.Path, "/api/"))
			return
		}
		h(&call{w: w, r: r, t: s.legacy}, strings.TrimPrefix(r.URL.Path, prefix))
	}))
}

func (s *Server) ListenAndServe() error {
	log.Printf("bucket-next listening on %s  shard=%d/%d  tenants=%s  legacy_tenant=%q  state=%s",
		s.httpsrv.Addr, s.cfg.ShardID, s.cfg.TotalShards, strings.Join(s.served, ","), s.cfg.LegacyTenant, s.cfg.StateDir)
	return s.httpsrv.ListenAndServe()
}

// Addr is the address ListenAndServe binds.
func (s *Server) Addr() string {
	return s.httpsrv.Addr
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpsrv.Shutdown(ctx)
}

// Close waits for every tenant's in-flight segment refills. Call it after Shutdown.
func (s *Server) Close() {
	for _, t := range s.tenants {
		t.alloc.Close()
	}
}

// Handler returns the underlying http.Handler. Useful for tests with httptest.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func methodOnly(method string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, "Method not allowed", "")
			return
		}
		h(w, r)
	}
}
