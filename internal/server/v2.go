package server

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/telcobright/bucket-next/internal/names"
)

// argKind is what follows a tenant-first road's name in the path.
type argKind int

const (
	argNone   argKind = iota // list, types: nothing
	argEntity                // the entity name (names.EntityRule)
	argID                    // a Snowflake id
)

// v2Road is one tenant-first road: /api/v2/{tenant}/{name}[/{arg}].
type v2Road struct {
	method string
	arg    argKind
	h      road
}

func (s *Server) v2Roads() map[string]v2Road {
	return map[string]v2Road{
		"next-id":         {http.MethodGet, argEntity, s.nextID},
		"next-batch":      {http.MethodGet, argEntity, s.nextBatch},
		"init":            {http.MethodPost, argEntity, s.initEntity},
		"reset":           {http.MethodPut, argEntity, s.reset},
		"status":          {http.MethodGet, argEntity, s.status},
		"segment-state":   {http.MethodGet, argEntity, s.segmentState},
		"list":            {http.MethodGet, argNone, s.list},
		"types":           {http.MethodGet, argNone, s.types},
		"parse-snowflake": {http.MethodGet, argID, s.parseSnowflake},
	}
}

// handleV2 serves /api/v2/{tenant}/{road}[/{arg}]. The tenant comes first and is checked
// first: missing, malformed or not served = 400 in words, never a default tenant. 400 and
// not 404 on purpose: 404 means "Entity not found", and a caller registers an entity on it.
func (s *Server) handleV2(w http.ResponseWriter, r *http.Request) {
	seg, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/v2/"), "/")
	if r.URL.Path == "/api/v2" {
		seg, rest = "", ""
	}
	t, ok := s.tenants[seg]
	if !ok {
		s.refuseTenant(w, seg)
		return
	}
	c := &call{w: w, r: r, t: t, v2: true}
	name, arg, hasArg := strings.Cut(rest, "/")
	rd, ok := s.roads[name]
	if !ok {
		c.refuse(http.StatusNotFound, "Not found", fmt.Sprintf("no road %q under /api/v2/%s/ — the roads are %s",
			name, t.name, strings.Join(s.roadNames(), ", ")))
		return
	}
	if r.Method != rd.method {
		w.Header().Set("Allow", rd.method)
		c.refuse(http.StatusMethodNotAllowed, "Method not allowed", "")
		return
	}
	switch rd.arg {
	case argNone:
		if hasArg {
			c.refuse(http.StatusNotFound, "Not found", fmt.Sprintf("/api/v2/%s/%s takes nothing after it", t.name, name))
			return
		}
	case argEntity:
		if arg != "" && !names.ValidEntity(arg) {
			c.refuse(http.StatusBadRequest, "Invalid entity name",
				fmt.Sprintf("entity '%s' breaks the rule %s (letters, digits, '.', '_', '-'; 1-64 characters)", arg, names.EntityRule))
			return
		}
	}
	rd.h(c, arg)
}

// refuseTenant answers a tenant-first road whose tenant segment serves no tenant.
func (s *Server) refuseTenant(w http.ResponseWriter, seg string) {
	served := strings.Join(s.served, ", ")
	_, isRoad := s.roads[seg]
	switch {
	case seg == "" || isRoad:
		writeError(w, http.StatusBadRequest, "Tenant is required",
			"the tenant comes first: /api/v2/{tenant}/next-id/{entity} (served tenants: "+served+")")
	case !names.ValidTenant(seg):
		writeError(w, http.StatusBadRequest, "Invalid tenant",
			fmt.Sprintf("tenant '%s' breaks the rule %s (tenant-master's tenant id: lowercase letters and digits, "+
				"a letter first, 2-16 characters; never case-folded)", seg, names.TenantRule))
	default:
		writeError(w, http.StatusBadRequest, "Unknown tenant",
			fmt.Sprintf("tenant '%s' is not served by this installation (served tenants: %s)", seg, served))
	}
}

func (s *Server) roadNames() []string {
	out := make([]string, 0, len(s.roads))
	for n := range s.roads {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
