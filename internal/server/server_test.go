package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/testutil"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// ---------- harness ----------

type harness struct {
	*httptest.Server
	codec *uuidgen.Codec
	cfg   *config.Config
}

func newTestServer(t *testing.T, shardID, totalShards int) *harness {
	return newTestServerSeg(t, shardID, totalShards, 10, 0.9)
}

func newTestServerSeg(t *testing.T, shardID, totalShards, segment int, watermark float64) *harness {
	t.Helper()
	cfg := testConfig(t, shardID, totalShards, segment, watermark)
	srv, codec := buildServer(t, cfg)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &harness{Server: ts, codec: codec, cfg: cfg}
}

// testConfig serves two tenants, btcl and acme; the pre-tenant roads act on btcl.
func testConfig(t *testing.T, shardID, totalShards, segment int, watermark float64) *config.Config {
	t.Helper()
	return &config.Config{
		ShardID:                shardID,
		TotalShards:            totalShards,
		StateDir:               filepath.Join(testutil.TempDir(t), "shard"),
		Tenants:                []string{"btcl", "acme"},
		LegacyTenant:           "btcl",
		SegmentSize:            segment,
		SegmentRefillWatermark: watermark,
		SnowflakeEpochMs:       1704067200000,
	}
}

// buildServer wires a real Server (a brand-new shard state, forge, codec) for cfg.
func buildServer(t *testing.T, cfg *config.Config) (*Server, *uuidgen.Codec) {
	t.Helper()
	codec, err := uuidgen.New(uuidgen.DefaultKey())
	if err != nil {
		t.Fatal(err)
	}
	l, err := state.Open(state.Options{
		Dir: cfg.StateDir,
		Identity: state.Identity{
			ShardID: cfg.ShardID, TotalShards: cfg.TotalShards,
			UUIDKeyFingerprint: codec.Fingerprint(), SnowflakeEpochMs: cfg.SnowflakeEpochMs,
		},
		Tenants:      cfg.Tenants,
		LegacyTenant: cfg.LegacyTenant,
		Create:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	fg, err := forge.New(cfg.ShardID, cfg.SnowflakeEpochMs, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := fg.EnableMark(l.Shard.SnowflakeMark(), l.Shard.RaiseSnowflakeMark); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, l, fg, codec)
	t.Cleanup(srv.Close)
	return srv, codec
}

func get(t *testing.T, ts *harness, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(body) > 0 {
		if err := json.Unmarshal(body, &m); err != nil {
			t.Fatalf("json decode (%s): %v\nbody: %s", path, err, body)
		}
	}
	return resp.StatusCode, m
}

func doJSON(t *testing.T, ts *harness, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, ts.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("json decode (%s %s): %v\nbody: %s", method, path, err, raw)
		}
	}
	return resp.StatusCode, m
}

// numStr renders an int (JSON number) or long (JSON string) value as a string.
func numStr(v any) string {
	switch x := v.(type) {
	case float64:
		return strconv.FormatInt(int64(x), 10)
	case string:
		return x
	}
	return fmt.Sprint(v)
}

// ---------- health / introspection ----------

func TestHealth(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	code, body := get(t, ts, "/health")
	if code != 200 || body["status"] != "healthy" {
		t.Fatalf("status %d body=%v", code, body)
	}
	if body["shard"].(float64) != 2 || body["totalShards"].(float64) != 3 {
		t.Errorf("shard fields: %v %v", body["shard"], body["totalShards"])
	}
}

func TestShardInfo_reportsClusterInvariants(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	code, body := get(t, ts, "/shard-info")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if body["shardId"].(float64) != 2 || body["totalShards"].(float64) != 3 {
		t.Errorf("body=%+v", body)
	}
	if body["uuidKeyFingerprint"] != ts.codec.Fingerprint() {
		t.Errorf("uuidKeyFingerprint=%v", body["uuidKeyFingerprint"])
	}
	if body["snowflakeEpochMs"].(float64) != 1704067200000 {
		t.Errorf("snowflakeEpochMs=%v", body["snowflakeEpochMs"])
	}
}

func TestTypes(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	_, body := get(t, ts, "/api/types")
	if types, _ := body["availableTypes"].([]any); len(types) != 7 {
		t.Errorf("availableTypes=%v", body["availableTypes"])
	}
	if _, ok := body["apiVersions"].(map[string]any); !ok {
		t.Error("apiVersions missing")
	}
}

// ---------- scalar ----------

func TestScalar_long_interleaved(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	for i, w := range []string{"2", "5", "8", "11", "14"} {
		code, body := get(t, ts, "/api/next-id/cdr?dataType=long")
		if code != 200 || body["value"] != w {
			t.Errorf("#%d status %d value=%v, want %q", i, code, body["value"], w)
		}
	}
}

func TestScalar_int_isJSONNumber(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	_, body := get(t, ts, "/api/next-id/counter?dataType=int")
	if v, ok := body["value"].(float64); !ok || v != 1 {
		t.Errorf("int value=%v (%T), want number 1", body["value"], body["value"])
	}
}

func TestScalar_everyType(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	for _, dt := range datatype.All {
		code, body := get(t, ts, "/api/next-id/e-"+string(dt)+"?dataType="+string(dt))
		if code != 200 || body["value"] == nil {
			t.Errorf("%s: status %d body=%v", dt, code, body)
		}
		if dt.IsUUID() {
			s, _ := body["value"].(string)
			if len(s) != dt.UUIDLength() {
				t.Errorf("%s: %q has %d chars", dt, s, len(s))
			}
		}
	}
}

func TestScalar_typeMismatch(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	get(t, ts, "/api/next-id/foo?dataType=long")
	code, body := get(t, ts, "/api/next-id/foo?dataType=int")
	if code != 400 || body["error"] != "Type mismatch" || body["registeredType"] != "long" {
		t.Errorf("status %d body=%v", code, body)
	}
}

func TestScalar_badDataType(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, q := range []string{"", "?dataType=bigint"} {
		if code, _ := get(t, ts, "/api/next-id/foo"+q); code != 400 {
			t.Errorf("%q: status %d, want 400", q, code)
		}
	}
}

// ---------- vector ----------

func TestVector_long_hasRange(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	code, body := get(t, ts, "/api/next-batch/sms?dataType=long&batchSize=5")
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	values, _ := body["values"].([]any)
	want := []string{"2", "5", "8", "11", "14"}
	for i, v := range values {
		if v != want[i] {
			t.Errorf("values[%d]=%v, want %s", i, v, want[i])
		}
	}
	rng, _ := body["range"].(map[string]any)
	if rng["first"] != "2" || rng["last"] != "14" || rng["step"].(float64) != 3 || rng["count"].(float64) != 5 {
		t.Errorf("range=%v", rng)
	}
}

func TestVector_everyType_uniqueAndContiguous(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	layout := struct{ k, n int64 }{2, 3}
	for _, dt := range datatype.All {
		entity := "v-" + string(dt)
		code, body := get(t, ts, "/api/next-batch/"+entity+"?dataType="+string(dt)+"&batchSize=500")
		if code != 200 {
			t.Fatalf("%s: status %d body=%v", dt, code, body)
		}
		values, _ := body["values"].([]any)
		if len(values) != 500 {
			t.Fatalf("%s: %d values", dt, len(values))
		}
		seen := map[string]bool{}
		for i, v := range values {
			s := numStr(v)
			if seen[s] {
				t.Fatalf("%s: duplicate %s", dt, s)
			}
			seen[s] = true
			switch {
			case dt.IsNumeric():
				if got, want := s, strconv.FormatInt(layout.k+int64(i)*layout.n, 10); got != want {
					t.Fatalf("%s[%d]=%s, want %s", dt, i, got, want)
				}
			case dt.IsUUID():
				// A uuid is the shard's i-th counter value, scrambled: it decodes back to it, with
				// the tenant's scramble (the pre-tenant roads act on legacy_tenant btcl).
				got, err := ts.codec.Decode(s, dt.UUIDLength(), uuidgen.TweakForTenant("btcl", entity))
				if want := layout.k + int64(i)*layout.n; err != nil || got != want {
					t.Fatalf("%s[%d]=%q decodes to %d (%v), want %d", dt, i, s, got, err, want)
				}
			}
		}
		_, hasRange := body["range"]
		if hasRange != dt.IsNumeric() {
			t.Errorf("%s: range present=%v", dt, hasRange)
		}
	}
}

func TestScalarAndVector_shareOneSequence(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	var got []string
	_, b := get(t, ts, "/api/next-id/x?dataType=int")
	got = append(got, numStr(b["value"]))
	_, b = get(t, ts, "/api/next-batch/x?dataType=int&batchSize=3")
	for _, v := range b["values"].([]any) {
		got = append(got, numStr(v))
	}
	_, b = get(t, ts, "/api/next-id/x?dataType=int")
	got = append(got, numStr(b["value"]))
	if strings.Join(got, ",") != "1,2,3,4,5" {
		t.Errorf("got %v, want 1..5", got)
	}
}

func TestVector_batchSizeOutOfRange(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, bs := range []string{"", "0", "-1", "10001", "x"} {
		if code, _ := get(t, ts, "/api/next-batch/x?dataType=long&batchSize="+bs); code != 400 {
			t.Errorf("batchSize=%q got %d, want 400", bs, code)
		}
	}
}

func TestExhausted_409(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	doJSON(t, ts, "POST", "/api/init/x", map[string]any{"dataType": "int", "startValue": datatype.MaxInt})
	if _, b := get(t, ts, "/api/next-id/x?dataType=int"); numStr(b["value"]) != "2147483647" {
		t.Fatalf("last int: %v", b)
	}
	code, body := get(t, ts, "/api/next-id/x?dataType=int")
	if code != 409 || body["error"] != "Range exhausted" {
		t.Errorf("status %d body=%v", code, body)
	}
}

// ---------- the bug this rewrite fixes, end to end ----------

func TestConcurrent_noDuplicatesOverHTTP(t *testing.T) {
	ts := newTestServerSeg(t, 2, 3, 10, 0.5)
	for _, dt := range []string{"long", "uuid8"} {
		var mu sync.Mutex
		seen := map[string]bool{}
		dups := 0
		var wg sync.WaitGroup
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				rng := rand.New(rand.NewSource(int64(g)))
				for i := 0; i < 60; i++ {
					var vals []any
					if rng.Intn(2) == 0 {
						resp, err := http.Get(ts.URL + "/api/next-id/hot?dataType=" + dt)
						if err != nil {
							t.Error(err)
							return
						}
						var b map[string]any
						json.NewDecoder(resp.Body).Decode(&b)
						resp.Body.Close()
						vals = []any{b["value"]}
					} else {
						resp, err := http.Get(fmt.Sprintf("%s/api/next-batch/hot?dataType=%s&batchSize=%d", ts.URL, dt, 1+rng.Intn(40)))
						if err != nil {
							t.Error(err)
							return
						}
						var b map[string]any
						json.NewDecoder(resp.Body).Decode(&b)
						resp.Body.Close()
						vals, _ = b["values"].([]any)
					}
					mu.Lock()
					for _, v := range vals {
						s := numStr(v)
						if seen[s] {
							dups++
						}
						seen[s] = true
					}
					mu.Unlock()
				}
			}(g)
		}
		wg.Wait()
		if dups > 0 {
			t.Fatalf("%s: %d duplicate IDs out of %d", dt, dups, len(seen)+dups)
		}
		// fresh entity name for the next type
		ts = newTestServerSeg(t, 2, 3, 10, 0.5)
	}
}

// ---------- init ----------

func TestInit_withStartValue_snapped(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	code, body := doJSON(t, ts, "POST", "/api/init/inv", map[string]any{"dataType": "long", "startValue": 1000})
	if code != 201 || body["adjusted"] != true || body["actualStartValue"] != "1001" || body["nextValue"] != "1001" {
		t.Fatalf("status %d body=%v", code, body)
	}
	if _, b := get(t, ts, "/api/next-id/inv?dataType=long"); b["value"] != "1001" {
		t.Errorf("first value after init: %v", b["value"])
	}
}

func TestInit_alreadyExists_409(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	doJSON(t, ts, "POST", "/api/init/x", map[string]any{"dataType": "long"})
	code, body := doJSON(t, ts, "POST", "/api/init/x", map[string]any{"dataType": "long"})
	if code != 409 || body["error"] != "Entity already exists" {
		t.Errorf("status %d body=%v", code, body)
	}
}

func TestInit_rejectsBadStartValues(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	cases := []map[string]any{
		{"dataType": "long", "startValue": 0},
		{"dataType": "long", "startValue": -50},
		{"dataType": "int", "startValue": datatype.MaxInt + 1},
		{"dataType": "uuid8", "startValue": 5},
		{"dataType": "snowflake", "startValue": 5},
	}
	for i, c := range cases {
		if code, body := doJSON(t, ts, "POST", fmt.Sprintf("/api/init/bad%d", i), c); code != 400 {
			t.Errorf("%v: status %d body=%v", c, code, body)
		}
	}
}

// ---------- reset ----------

func TestReset_forwardSnapped(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	get(t, ts, "/api/next-id/x?dataType=long")
	code, body := doJSON(t, ts, "PUT", "/api/reset/x", map[string]any{"value": 5000})
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	if reset := body["reset"].(map[string]any); reset["actualValue"] != "5000" {
		t.Errorf("actualValue=%v", reset["actualValue"])
	}
	if _, b := get(t, ts, "/api/next-id/x?dataType=long"); b["value"] != "5000" {
		t.Errorf("after reset: %v", b["value"])
	}
}

func TestReset_backwardNeedsForce(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for i := 0; i < 5; i++ {
		get(t, ts, "/api/next-id/x?dataType=int") // 1..5 issued
	}
	code, body := doJSON(t, ts, "PUT", "/api/reset/x", map[string]any{"value": 2})
	if code != 409 || body["error"] != "Backward reset refused" {
		t.Fatalf("status %d body=%v", code, body)
	}
	if _, b := get(t, ts, "/api/next-id/x?dataType=int"); numStr(b["value"]) != "6" {
		t.Errorf("a refused reset must change nothing: next=%v", b["value"])
	}
	code, body = doJSON(t, ts, "PUT", "/api/reset/x", map[string]any{"value": 2, "force": true})
	if code != 200 || body["reset"].(map[string]any)["forced"] != true {
		t.Fatalf("forced: status %d body=%v", code, body)
	}
	if _, b := get(t, ts, "/api/next-id/x?dataType=int"); numStr(b["value"]) != "2" {
		t.Errorf("after forced reset: %v", b["value"])
	}
}

func TestReset_rejects(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	get(t, ts, "/api/next-id/n?dataType=long")
	get(t, ts, "/api/next-id/u?dataType=uuid12")
	cases := []struct {
		path string
		body map[string]any
		code int
	}{
		{"/api/reset/missing", map[string]any{"value": 5}, 404},
		{"/api/reset/u", map[string]any{"value": 5}, 400},
		{"/api/reset/n", map[string]any{"value": 0}, 400},
		{"/api/reset/n", map[string]any{"value": -3}, 400},
		{"/api/reset/n", map[string]any{}, 400},
	}
	for _, c := range cases {
		if code, body := doJSON(t, ts, "PUT", c.path, c.body); code != c.code {
			t.Errorf("%s %v: status %d, want %d (%v)", c.path, c.body, code, c.code, body)
		}
	}
}

// ---------- status / list / segment-state / parse ----------

func TestStatus(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	get(t, ts, "/api/next-id/n?dataType=long")
	get(t, ts, "/api/next-id/n?dataType=long")
	get(t, ts, "/api/next-id/s?dataType=snowflake")
	if _, b := get(t, ts, "/api/status/n"); b["currentValue"] != "2" || b["nextValue"] != "3" {
		t.Errorf("numeric status: %v", b)
	}
	if _, b := get(t, ts, "/api/status/s"); !strings.Contains(fmt.Sprint(b["currentValue"]), "Snowflake") {
		t.Errorf("snowflake status: %v", b)
	}
	if code, _ := get(t, ts, "/api/status/missing"); code != 404 {
		t.Errorf("missing: %d", code)
	}
}

func TestList(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for _, q := range []string{"a?dataType=long", "b?dataType=uuid12", "c?dataType=snowflake"} {
		get(t, ts, "/api/next-id/"+q)
	}
	_, body := get(t, ts, "/api/list")
	if entities, _ := body["entities"].([]any); len(entities) != 3 {
		t.Errorf("entities=%v", body["entities"])
	}
}

func TestSegmentState(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	for i := 0; i < 3; i++ {
		get(t, ts, "/api/next-id/x?dataType=long")
	}
	code, body := get(t, ts, "/api/segment-state/x")
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	seg, _ := body["segment"].(map[string]any)
	if seg["size"].(float64) != 10 || seg["issued"].(float64) != 3 || seg["remaining"].(float64) != 7 {
		t.Errorf("seg=%v", seg)
	}
	get(t, ts, "/api/next-id/u?dataType=uuid8")
	if code, _ := get(t, ts, "/api/segment-state/u"); code != 200 {
		t.Errorf("uuid entities have reservations too: %d", code)
	}
	get(t, ts, "/api/next-id/s?dataType=snowflake")
	if code, _ := get(t, ts, "/api/segment-state/s"); code != 400 {
		t.Errorf("snowflake: %d, want 400", code)
	}
}

func TestParseSnowflake(t *testing.T) {
	ts := newTestServer(t, 3, 5)
	_, b := get(t, ts, "/api/next-id/e?dataType=snowflake")
	id := b["value"].(string)
	code, body := get(t, ts, "/api/parse-snowflake/"+id)
	if code != 200 || body["shardId"].(float64) != 3 || body["id"] != id {
		t.Errorf("status %d body=%v", code, body)
	}
	if code, _ := get(t, ts, "/api/parse-snowflake/not-a-number"); code != 400 {
		t.Errorf("invalid: %d", code)
	}
}

// ---------- routing ----------

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	resp, err := http.Post(ts.URL+"/health", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

func TestUnknownPath_404(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	resp, err := http.Get(ts.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}

func TestUUID_twoEntitiesNeverShareASequence(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	_, a := get(t, ts, "/api/next-batch/session?dataType=uuid16&batchSize=200")
	_, b := get(t, ts, "/api/next-batch/token?dataType=uuid16&batchSize=200")
	seen := map[string]bool{}
	for _, v := range a["values"].([]any) {
		seen[v.(string)] = true
	}
	for _, v := range b["values"].([]any) {
		if seen[v.(string)] {
			t.Fatalf("%s issued to both entities", v)
		}
	}
}

// ---------- listen_address ----------

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func nonLoopbackIPv4s() []string {
	addrs, _ := net.InterfaceAddrs()
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func TestListenAddress_emptyMeansAllInterfaces(t *testing.T) {
	cfg := testConfig(t, 1, 1, 10, 0.9)
	cfg.ListenPort = 7001
	srv, _ := buildServer(t, cfg)
	if srv.Addr() != ":7001" {
		t.Errorf("Addr()=%q, want \":7001\"", srv.Addr())
	}
}

// TestListenAddress_bindsOnlyThatAddress starts the real server bound to 127.0.0.1 and checks
// it answers there and refuses connections on every other local IPv4 address.
func TestListenAddress_bindsOnlyThatAddress(t *testing.T) {
	port := freeLoopbackPort(t)
	cfg := testConfig(t, 1, 1, 10, 0.9)
	cfg.ListenAddress = "127.0.0.1"
	cfg.ListenPort = port
	srv, _ := buildServer(t, cfg)
	if want := fmt.Sprintf("127.0.0.1:%d", port); srv.Addr() != want {
		t.Fatalf("Addr()=%q, want %q", srv.Addr(), want)
	}
	go srv.ListenAndServe()
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/health", port))
		if err == nil {
			resp.Body.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not reachable on 127.0.0.1:%d: %v", port, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	others := nonLoopbackIPv4s()
	for _, ip := range others {
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), 300*time.Millisecond)
		if err == nil {
			c.Close()
			t.Errorf("reachable on %s:%d — the server bound more than 127.0.0.1", ip, port)
		}
	}
	t.Logf("checked %d non-loopback addresses: %v", len(others), others)
}
