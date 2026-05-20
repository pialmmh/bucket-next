package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/state"
)

type Server struct {
	cfg       *config.Config
	state     *state.Store
	forge     *forge.Forge
	allocator *allocator.Allocator
	startedAt time.Time
	mux       *http.ServeMux
	httpsrv   *http.Server
}

func New(cfg *config.Config, st *state.Store, f *forge.Forge, alloc *allocator.Allocator) *Server {
	s := &Server{
		cfg:       cfg,
		state:     st,
		forge:     f,
		allocator: alloc,
		startedAt: time.Now(),
		mux:       http.NewServeMux(),
	}
	s.routes()
	s.httpsrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.ListenPort),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("/health", methodOnly(http.MethodGet, s.handleHealth))
	s.mux.HandleFunc("/shard-info", methodOnly(http.MethodGet, s.handleShardInfo))
	s.mux.HandleFunc("/api/types", methodOnly(http.MethodGet, s.handleTypes))
	s.mux.HandleFunc("/api/list", methodOnly(http.MethodGet, s.handleList))
	s.mux.HandleFunc("/api/next-id/", methodOnly(http.MethodGet, s.handleNextID))
	s.mux.HandleFunc("/api/next-batch/", methodOnly(http.MethodGet, s.handleNextBatch))
	s.mux.HandleFunc("/api/init/", methodOnly(http.MethodPost, s.handleInit))
	s.mux.HandleFunc("/api/reset/", methodOnly(http.MethodPut, s.handleReset))
	s.mux.HandleFunc("/api/status/", methodOnly(http.MethodGet, s.handleStatus))
	s.mux.HandleFunc("/api/segment-state/", methodOnly(http.MethodGet, s.handleSegmentState))
	s.mux.HandleFunc("/api/parse-snowflake/", methodOnly(http.MethodGet, s.handleParseSnowflake))
}

func (s *Server) ListenAndServe() error {
	log.Printf("bucket-next listening on :%d  shard=%d/%d  state=%s",
		s.cfg.ListenPort, s.cfg.ShardID, s.cfg.TotalShards, s.cfg.StatePath)
	return s.httpsrv.ListenAndServe()
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpsrv.Shutdown(ctx)
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
