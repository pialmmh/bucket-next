package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// newTestServerCfg serves cfg (see testConfig: tenants btcl and acme).
func newTestServerCfg(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	srv, codec := buildServer(t, cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{Server: ts, codec: codec, cfg: cfg}
}

// v2RoadCalls lists every tenant-first road with a call that succeeds on a fresh tenant ("%s"
// is the tenant).
var v2RoadCalls = []struct {
	method, path string
	body         any
}{
	{"GET", "/api/v2/%s/next-id/cdr?dataType=long", nil},
	{"GET", "/api/v2/%s/next-batch/sms?dataType=long&batchSize=3", nil},
	{"POST", "/api/v2/%s/init/inv", map[string]any{"dataType": "long", "startValue": 1000}},
	{"PUT", "/api/v2/%s/reset/cdr", map[string]any{"value": 5000}},
	{"GET", "/api/v2/%s/status/cdr", nil},
	{"GET", "/api/v2/%s/segment-state/cdr", nil},
	{"GET", "/api/v2/%s/list", nil},
	{"GET", "/api/v2/%s/types", nil},
	{"GET", "/api/v2/%s/parse-snowflake/1", nil},
}

func TestV2_everyRoadRefusesAMissingMalformedOrUnknownTenant(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, rc := range v2RoadCalls {
		bad := map[string]string{
			strings.Replace(rc.path, "%s/", "", 1): "Tenant is required", // the road where the tenant goes
			fmt.Sprintf(rc.path, "BTCL"):           "Invalid tenant",
			fmt.Sprintf(rc.path, "bt_cl"):          "Invalid tenant",
			fmt.Sprintf(rc.path, "b"):              "Invalid tenant",
			fmt.Sprintf(rc.path, "zz9"):            "Unknown tenant",
		}
		for path, word := range bad {
			code, body := doJSON(t, ts, rc.method, path, rc.body)
			if code != 400 || body["error"] != word {
				t.Errorf("%s %s: got %d %v, want 400 %q", rc.method, path, code, body["error"], word)
			}
			if _, named := body["tenant"]; named {
				t.Errorf("%s %s: a refused tenant must not be named as served: %v", rc.method, path, body)
			}
		}
	}
	for _, path := range []string{"/api/v2", "/api/v2/"} {
		if code, body := get(t, ts, path); code != 400 || body["error"] != "Tenant is required" {
			t.Errorf("%s: got %d %v", path, code, body)
		}
	}
	// nothing was registered by any of it
	if _, b := get(t, ts, "/api/v2/btcl/list"); len(b["entities"].([]any)) != 0 {
		t.Errorf("refused calls registered entities: %v", b["entities"])
	}
}

func TestV2_everyAnswerNamesItsTenant(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, rc := range v2RoadCalls {
		path := fmt.Sprintf(rc.path, "acme")
		code, body := doJSON(t, ts, rc.method, path, rc.body)
		if code >= 300 || body["tenant"] != "acme" {
			t.Errorf("%s %s: %d tenant=%v body=%v", rc.method, path, code, body["tenant"], body)
		}
	}
	// a refusal after the tenant is known names it too
	code, body := get(t, ts, "/api/v2/acme/status/missing")
	if code != 404 || body["error"] != "Entity not found" || body["tenant"] != "acme" {
		t.Errorf("status of a missing entity: %d %v", code, body)
	}
	// and the pre-tenant roads answer as before: no tenant field
	if _, b := get(t, ts, "/api/next-id/cdr?dataType=long"); b["tenant"] != nil {
		t.Errorf("a v1 answer names a tenant: %v", b)
	}
}

func TestV2_tenantsHaveTheirOwnCounters(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	var btcl, acme []string
	for i := 0; i < 3; i++ {
		_, b := get(t, ts, "/api/v2/btcl/next-id/partner?dataType=int")
		btcl = append(btcl, numStr(b["value"]))
	}
	for i := 0; i < 2; i++ {
		_, b := get(t, ts, "/api/v2/acme/next-id/partner?dataType=int")
		acme = append(acme, numStr(b["value"]))
	}
	if strings.Join(btcl, ",") != "2,5,8" || strings.Join(acme, ",") != "2,5" {
		t.Errorf("btcl %v, acme %v: each tenant counts on its own from the shard's first value", btcl, acme)
	}
	// one tenant's entity, type and list are its own
	get(t, ts, "/api/v2/acme/next-id/only-acme?dataType=long")
	if code, _ := get(t, ts, "/api/v2/btcl/status/only-acme"); code != 404 {
		t.Errorf("btcl sees acme's entity: %d", code)
	}
	if code, _ := get(t, ts, "/api/v2/btcl/next-id/only-acme?dataType=int"); code != 200 {
		t.Errorf("btcl may bind the same name to another type: %d", code)
	}
	_, l := get(t, ts, "/api/v2/acme/list")
	if n := len(l["entities"].([]any)); n != 2 {
		t.Errorf("acme lists %d entities, want 2", n)
	}
}

func TestV2_uuidStringsAreUnrelatedAcrossTenants(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	strs := map[string][]string{}
	for _, tn := range []string{"btcl", "acme"} {
		_, b := get(t, ts, "/api/v2/"+tn+"/next-batch/session?dataType=uuid16&batchSize=200")
		for i, v := range b["values"].([]any) {
			s := v.(string)
			strs[tn] = append(strs[tn], s)
			got, err := ts.codec.Decode(s, 16, uuidgen.TweakForTenant(tn, "session"))
			if err != nil || got != int64(i+1) {
				t.Fatalf("%s[%d]=%s decodes to %d (%v) with its tenant's scramble", tn, i, s, got, err)
			}
		}
	}
	seen := map[string]bool{}
	for _, s := range strs["btcl"] {
		seen[s] = true
	}
	for _, s := range strs["acme"] {
		if seen[s] {
			t.Fatalf("both tenants issued %s", s)
		}
	}
}

func TestV2_andV1DrawFromTheLegacyTenantsOneCounter(t *testing.T) {
	ts := newTestServer(t, 1, 2)
	doJSON(t, ts, "POST", "/api/init/partner.btcl", map[string]any{"dataType": "int", "startValue": 2007})
	var got []string
	for i := 0; i < 4; i++ {
		_, a := get(t, ts, "/api/next-id/partner.btcl?dataType=int")
		_, b := get(t, ts, "/api/v2/btcl/next-id/partner.btcl?dataType=int")
		got = append(got, numStr(a["value"]), numStr(b["value"]))
	}
	if strings.Join(got, ",") != "2007,2009,2011,2013,2015,2017,2019,2021" {
		t.Errorf("v1 and v2 on btcl: %v", got)
	}
	if _, b := get(t, ts, "/api/v2/acme/next-id/partner.btcl?dataType=int"); numStr(b["value"]) != "1" {
		t.Errorf("acme's partner.btcl is its own counter: %v", b["value"])
	}
}

func TestV2_aMissingEntityIs404AndAnUnknownTenantIs400(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	if code, b := get(t, ts, "/api/v2/btcl/status/missing"); code != 404 || b["error"] != "Entity not found" {
		t.Errorf("missing entity: %d %v", code, b)
	}
	if code, b := get(t, ts, "/api/v2/nobody/status/missing"); code != 400 || b["error"] != "Unknown tenant" {
		t.Errorf("unknown tenant: %d %v", code, b)
	}
}

// TestV2_theSeedClientsSequence replays seed-idgen's ShardCounter.raiseAbove + mint on the
// tenant-first roads: status 404 → init above the highest in use → next-id; then, once
// registered, status → reset forward when the counter would hand out an id in use.
func TestV2_theSeedClientsSequence(t *testing.T) {
	ts := newTestServer(t, 1, 2)
	if code, b := get(t, ts, "/api/v2/btcl/status/partner.btcl"); code != 404 || b["error"] != "Entity not found" {
		t.Fatalf("first status: %d %v", code, b)
	}
	code, b := doJSON(t, ts, "POST", "/api/v2/btcl/init/partner.btcl", map[string]any{"dataType": "int", "startValue": 5001})
	if code != 201 || numStr(b["nextValue"]) != "5001" {
		t.Fatalf("init: %d %v", code, b)
	}
	if code, b := doJSON(t, ts, "POST", "/api/v2/btcl/init/partner.btcl", map[string]any{"dataType": "int", "startValue": 5001}); code != 409 || b["error"] != "Entity already exists" {
		t.Errorf("a racing init: %d %v", code, b)
	}
	if _, b := get(t, ts, "/api/v2/btcl/next-id/partner.btcl?dataType=int"); numStr(b["value"]) != "5001" {
		t.Errorf("first id: %v", b["value"])
	}
	// the tree now holds 9000: the counter is moved forward past it
	_, st := get(t, ts, "/api/v2/btcl/status/partner.btcl")
	if numStr(st["nextValue"]) != "5003" {
		t.Fatalf("status nextValue: %v", st["nextValue"])
	}
	if code, b := doJSON(t, ts, "PUT", "/api/v2/btcl/reset/partner.btcl", map[string]any{"value": 9001}); code != 200 {
		t.Fatalf("reset: %d %v", code, b)
	}
	if code, b := doJSON(t, ts, "PUT", "/api/v2/btcl/reset/partner.btcl", map[string]any{"value": 9001}); code != 200 {
		t.Errorf("a reset to where it is: %d %v", code, b)
	}
	if code, b := doJSON(t, ts, "PUT", "/api/v2/btcl/reset/partner.btcl", map[string]any{"value": 7001}); code != 409 || b["error"] != "Backward reset refused" {
		t.Errorf("a racing backward reset: %d %v", code, b)
	}
	if _, b := get(t, ts, "/api/v2/btcl/next-id/partner.btcl?dataType=int"); numStr(b["value"]) != "9001" {
		t.Errorf("after the move: %v", b["value"])
	}
}

func TestV2_entityNamesAreChecked(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, e := range []string{"a%20b", strings.Repeat("x", 65), "a/b", "a%2Fb"} {
		if code, b := get(t, ts, "/api/v2/btcl/next-id/"+e+"?dataType=long"); code != 400 || b["error"] != "Invalid entity name" {
			t.Errorf("entity %q: %d %v", e, code, b)
		}
	}
	if code, b := get(t, ts, "/api/v2/btcl/next-id/?dataType=long"); code != 400 || b["error"] != "Entity name is required" {
		t.Errorf("no entity: %d %v", code, b)
	}
	if code, _ := get(t, ts, "/api/v2/btcl/next-id/"+strings.Repeat("x", 64)+"?dataType=long"); code != 200 {
		t.Errorf("a 64-character name: %d", code)
	}
}

func TestV2_unknownRoadsAndMethods(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	if code, b := get(t, ts, "/api/v2/btcl/nope"); code != 404 || b["error"] != "Not found" || b["tenant"] != "btcl" {
		t.Errorf("unknown road: %d %v", code, b)
	}
	if code, b := get(t, ts, "/api/v2/btcl/list/extra"); code != 404 || b["error"] != "Not found" {
		t.Errorf("list with a tail: %d %v", code, b)
	}
	req, _ := http.NewRequest("POST", ts.URL+"/api/v2/btcl/next-id/x?dataType=long", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Allow") != "GET" {
		t.Errorf("POST next-id: %d Allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

func TestV1_switchedOffRefusesEveryRoadAndMintsNothing(t *testing.T) {
	cfg := testConfig(t, 1, 1, 10, 0.9)
	cfg.LegacyTenant = ""
	ts := newTestServerCfg(t, cfg)
	roads := []struct{ method, path string }{
		{"GET", "/api/next-id/x?dataType=long"},
		{"GET", "/api/next-batch/x?dataType=long&batchSize=2"},
		{"POST", "/api/init/x"},
		{"PUT", "/api/reset/x"},
		{"GET", "/api/status/x"},
		{"GET", "/api/segment-state/x"},
		{"GET", "/api/list"},
		{"GET", "/api/types"},
		{"GET", "/api/parse-snowflake/1"},
	}
	for _, rd := range roads {
		code, b := doJSON(t, ts, rd.method, rd.path, map[string]any{"dataType": "long", "value": 5})
		if code != 400 || b["error"] != "Tenant is required" || !strings.Contains(fmt.Sprint(b["message"]), "/api/v2/{tenant}/") {
			t.Errorf("%s %s: %d %v", rd.method, rd.path, code, b)
		}
	}
	for _, tn := range []string{"btcl", "acme"} {
		if _, b := get(t, ts, "/api/v2/"+tn+"/list"); len(b["entities"].([]any)) != 0 {
			t.Errorf("%s has entities after refused calls: %v", tn, b["entities"])
		}
	}
	if _, b := get(t, ts, "/shard-info"); b["legacyTenant"] != nil || numStr(b["v1Calls"]) != "9" {
		t.Errorf("shard-info: legacyTenant=%v v1Calls=%v", b["legacyTenant"], b["v1Calls"])
	}
}

func TestShardInfo_tenantsAndTheV1Count(t *testing.T) {
	ts := newTestServer(t, 1, 2)
	for i := 0; i < 3; i++ {
		get(t, ts, "/api/next-id/cdr?dataType=long")
	}
	get(t, ts, "/api/v2/btcl/next-id/cdr?dataType=long") // not counted
	_, b := get(t, ts, "/shard-info")
	if fmt.Sprint(b["tenants"]) != "[acme btcl]" || b["legacyTenant"] != "btcl" {
		t.Errorf("tenants=%v legacyTenant=%v", b["tenants"], b["legacyTenant"])
	}
	callers, _ := b["v1Callers"].(map[string]any)
	if numStr(b["v1Calls"]) != "3" || numStr(callers["127.0.0.1"]) != "3" {
		t.Errorf("v1Calls=%v v1Callers=%v", b["v1Calls"], b["v1Callers"])
	}
	if _, h := get(t, ts, "/health"); fmt.Sprint(h["tenants"]) != "[acme btcl]" {
		t.Errorf("health tenants=%v", h["tenants"])
	}
}

func TestV2_typesAndParseSnowflake(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	_, ty := get(t, ts, "/api/v2/btcl/types")
	if v := ty["apiVersions"].(map[string]any); !strings.Contains(fmt.Sprint(v["scalar"]), "/api/v2/{tenant}/next-id/") {
		t.Errorf("types apiVersions: %v", v)
	}
	_, b := get(t, ts, "/api/v2/btcl/next-id/ev?dataType=snowflake")
	code, p := get(t, ts, "/api/v2/acme/parse-snowflake/"+b["value"].(string))
	if code != 200 || p["id"] != b["value"] || numStr(p["shardId"]) != "1" || p["tenant"] != "acme" {
		t.Errorf("parse-snowflake: %d %v", code, p)
	}
}

// TestV2_concurrentTenantsNoDuplicates hammers two tenants' counters at once with tiny
// segments, scalar and vector mixed: every ID is issued once within its tenant.
func TestV2_concurrentTenantsNoDuplicates(t *testing.T) {
	ts := newTestServerSeg(t, 2, 3, 7, 0.5)
	const workers, rounds = 6, 60
	var mu sync.Mutex
	seen := map[string]map[string]bool{"btcl": {}, "acme": {}}
	var wg sync.WaitGroup
	for _, tn := range []string{"btcl", "acme"} {
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(tn string, w int) {
				defer wg.Done()
				for i := 0; i < rounds; i++ {
					var vals []string
					if (w+i)%3 == 0 {
						_, b := get(t, ts, "/api/v2/"+tn+"/next-batch/cdr?dataType=long&batchSize=5")
						for _, v := range b["values"].([]any) {
							vals = append(vals, numStr(v))
						}
					} else {
						_, b := get(t, ts, "/api/v2/"+tn+"/next-id/cdr?dataType=long")
						vals = append(vals, numStr(b["value"]))
					}
					mu.Lock()
					for _, v := range vals {
						if seen[tn][v] {
							t.Errorf("%s issued %s twice", tn, v)
						}
						seen[tn][v] = true
					}
					mu.Unlock()
				}
			}(tn, w)
		}
	}
	wg.Wait()
	want := 0
	for w := 0; w < workers; w++ {
		for i := 0; i < rounds; i++ {
			if (w+i)%3 == 0 {
				want += 5
			} else {
				want++
			}
		}
	}
	for tn, s := range seen {
		if len(s) != want {
			t.Errorf("%s: %d distinct IDs, want %d", tn, len(s), want)
		}
	}
}
