package server

import (
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// v1 registers a pre-tenant road (no tenant in the path). With legacy_tenant set it answers
// exactly as before tenancy, on that tenant; without, it refuses in words, naming the
// tenant-first road to call. Every call is counted per caller, served or refused, so an
// operator sees who has not moved to /api/v2/ yet.
func (s *Server) v1(prefix, method string, h road) {
	s.mux.HandleFunc(prefix, methodOnly(method, func(w http.ResponseWriter, r *http.Request) {
		s.v1Calls.note(r, s.legacy)
		if s.legacy == nil {
			writeError(w, http.StatusBadRequest, "Tenant is required",
				"this installation serves tenants by name: call /api/v2/{tenant}/"+strings.TrimPrefix(r.URL.Path, "/api/"))
			return
		}
		h(&call{w: w, r: r, t: s.legacy}, strings.TrimPrefix(r.URL.Path, prefix))
	}))
}

// maxV1Callers bounds the per-caller table; callers past it are counted as "other".
const maxV1Callers = 64

// v1Calls counts the pre-tenant roads' calls per caller address.
type v1Calls struct {
	mu      sync.Mutex
	total   int64
	callers map[string]*v1Caller
}

type v1Caller struct {
	calls    int64
	loggedAt time.Time
}

// note counts one call and logs it once per caller per hour.
func (v *v1Calls) note(r *http.Request, legacy *tenant) {
	ip := r.RemoteAddr
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ip = host
	}
	v.mu.Lock()
	v.total++
	c, ok := v.callers[ip]
	if !ok && len(v.callers) >= maxV1Callers {
		ip = "other"
		c, ok = v.callers[ip]
	}
	if !ok {
		c = &v1Caller{}
		v.callers[ip] = c
	}
	c.calls++
	logIt := c.loggedAt.IsZero() || time.Since(c.loggedAt) >= time.Hour
	if logIt {
		c.loggedAt = time.Now()
	}
	n := c.calls
	v.mu.Unlock()
	if !logIt {
		return
	}
	if legacy == nil {
		log.Printf("pre-tenant road %s %s from %s REFUSED (no legacy_tenant; %d such calls from it so far) — it must call /api/v2/{tenant}/...",
			r.Method, r.URL.Path, ip, n)
		return
	}
	log.Printf("pre-tenant road %s %s from %s served on legacy_tenant %s (%d such calls from it so far) — move it to /api/v2/%s/...",
		r.Method, r.URL.Path, ip, legacy.name, n, legacy.name)
}

func (v *v1Calls) snapshot() (int64, map[string]int64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make(map[string]int64, len(v.callers))
	for ip, c := range v.callers {
		out[ip] = c.calls
	}
	return v.total, out
}
