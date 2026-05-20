package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/config"
	"github.com/telcobright/bucket-next/internal/forge"
	"github.com/telcobright/bucket-next/internal/state"
)

// ---------- harness ----------

func newTestServer(t *testing.T, shardID, totalShards int) *httptest.Server {
	t.Helper()
	cfg := &config.Config{
		ShardID:                shardID,
		TotalShards:            totalShards,
		ListenPort:             0,
		StatePath:              filepath.Join(t.TempDir(), "s.json"),
		SegmentSize:            10,
		SegmentRefillWatermark: 0.9,
		SnowflakeEpochMs:       1704067200000,
	}
	st, err := state.Open(cfg.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	fg, err := forge.New(shardID, cfg.SnowflakeEpochMs, 0)
	if err != nil {
		t.Fatal(err)
	}
	alloc := allocator.New(allocator.Config{
		ShardID:     shardID,
		TotalShards: totalShards,
		SegmentSize: cfg.SegmentSize,
		Watermark:   cfg.SegmentRefillWatermark,
		Store:       st,
	})
	t.Cleanup(alloc.Close) // wait for refill goroutines before tempdir cleanup
	srv := New(cfg, st, fg, alloc)
	return httptest.NewServer(srv.Handler())
}

func get(t *testing.T, ts *httptest.Server, path string) (int, map[string]any) {
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

func doJSON(t *testing.T, ts *httptest.Server, method, path string, body any) (int, map[string]any) {
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

// ---------- health / introspection ----------

func TestHealth(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	code, body := get(t, ts, "/health")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if body["status"] != "healthy" {
		t.Errorf("status=%v", body["status"])
	}
	if body["shard"].(float64) != 2 || body["totalShards"].(float64) != 3 {
		t.Errorf("shard fields: %v %v", body["shard"], body["totalShards"])
	}
	if _, ok := body["snowflakeStats"]; !ok {
		t.Error("snowflakeStats missing")
	}
}

func TestShardInfo(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	code, body := get(t, ts, "/shard-info")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if body["shardId"].(float64) != 2 || body["totalShards"].(float64) != 3 {
		t.Errorf("body=%+v", body)
	}
	if body["status"] != "active" {
		t.Errorf("status=%v", body["status"])
	}
}

func TestTypes(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, body := get(t, ts, "/api/types")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	types, _ := body["availableTypes"].([]any)
	if len(types) != 7 {
		t.Errorf("len(availableTypes)=%d, want 7", len(types))
	}
	if _, ok := body["snowflakeInfo"].(map[string]any); !ok {
		t.Error("snowflakeInfo missing")
	}
}

// ---------- next-id ----------

func TestNextID_long_interleaved(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	want := []string{"2", "5", "8", "11", "14"}
	for i, w := range want {
		code, body := get(t, ts, "/api/next-id/cdr?dataType=long")
		if code != 200 {
			t.Fatalf("#%d status %d body=%v", i, code, body)
		}
		if body["value"] != w {
			t.Errorf("#%d value=%v, want %q", i, body["value"], w)
		}
		if body["shard"].(float64) != 2 {
			t.Errorf("#%d shard=%v", i, body["shard"])
		}
	}
}

func TestNextID_int_returnedAsNumber(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, body := get(t, ts, "/api/next-id/counter?dataType=int")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	// int is a JSON number, not a string.
	v, ok := body["value"].(float64)
	if !ok || v != 1 {
		t.Errorf("int value=%v (%T), want 1 (number)", body["value"], body["value"])
	}
}

func TestNextID_snowflake_isDecimalString(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	code, body := get(t, ts, "/api/next-id/evt?dataType=snowflake")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	s, ok := body["value"].(string)
	if !ok || s == "" {
		t.Errorf("snowflake value=%v (%T), want non-empty string", body["value"], body["value"])
	}
}

func TestNextID_uuid12_returnsLength12String(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, body := get(t, ts, "/api/next-id/tok?dataType=uuid12")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	s, _ := body["value"].(string)
	if len(s) != 12 {
		t.Errorf("uuid12 len=%d, want 12 (val=%q)", len(s), s)
	}
}

func TestNextID_typeMismatch(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	// First call binds as long.
	if code, _ := get(t, ts, "/api/next-id/foo?dataType=long"); code != 200 {
		t.Fatalf("first call status=%d", code)
	}
	// Second call with different type → 400.
	code, body := get(t, ts, "/api/next-id/foo?dataType=int")
	if code != 400 {
		t.Fatalf("status %d, want 400", code)
	}
	if body["error"] != "Type mismatch" {
		t.Errorf("error=%v", body["error"])
	}
	if body["registeredType"] != "long" {
		t.Errorf("registeredType=%v", body["registeredType"])
	}
}

func TestNextID_missingDataType(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, _ := get(t, ts, "/api/next-id/foo")
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
}

func TestNextID_invalidDataType(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, _ := get(t, ts, "/api/next-id/foo?dataType=bigint")
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
}

// ---------- next-batch ----------

func TestNextBatch_long(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	code, body := get(t, ts, "/api/next-batch/sms?dataType=long&batchSize=5")
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	values, _ := body["values"].([]any)
	if len(values) != 5 {
		t.Fatalf("len(values)=%d, want 5", len(values))
	}
	want := []string{"2", "5", "8", "11", "14"}
	for i, v := range values {
		if v != want[i] {
			t.Errorf("values[%d]=%v, want %q", i, v, want[i])
		}
	}
	if body["startValue"] != "2" || body["endValue"] != "14" {
		t.Errorf("start/end = %v/%v", body["startValue"], body["endValue"])
	}
}

func TestNextBatch_snowflake_uniqueness(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, body := get(t, ts, "/api/next-batch/evt?dataType=snowflake&batchSize=200")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	values, _ := body["values"].([]any)
	if len(values) != 200 {
		t.Fatalf("len=%d", len(values))
	}
	seen := make(map[string]struct{}, 200)
	for _, v := range values {
		s := v.(string)
		if _, dup := seen[s]; dup {
			t.Fatalf("duplicate: %s", s)
		}
		seen[s] = struct{}{}
	}
}

func TestNextBatch_batchSizeOutOfRange(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	for _, bs := range []string{"0", "-1", "10001"} {
		code, _ := get(t, ts, "/api/next-batch/x?dataType=long&batchSize="+bs)
		if code != 400 {
			t.Errorf("batchSize=%s got %d, want 400", bs, code)
		}
	}
}

// ---------- init / reset / status ----------

func TestInit_withStartValue_snapped(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	// 1000 mod 3 = 1; shard 2 wants residue 2 → snap to 1001.
	code, body := doJSON(t, ts, "POST", "/api/init/inv",
		map[string]any{"dataType": "long", "startValue": 1000})
	if code != 201 {
		t.Fatalf("status %d body=%v", code, body)
	}
	if body["adjusted"] != true {
		t.Errorf("adjusted=%v, want true", body["adjusted"])
	}
	if body["actualStartValue"] != "1001" {
		t.Errorf("actualStartValue=%v, want 1001", body["actualStartValue"])
	}
	if body["nextValue"] != "1001" {
		t.Errorf("nextValue=%v, want 1001", body["nextValue"])
	}

	// Next /next-id should return 1001.
	_, b := get(t, ts, "/api/next-id/inv?dataType=long")
	if b["value"] != "1001" {
		t.Errorf("after init+next-id, value=%v, want 1001", b["value"])
	}
}

func TestInit_alreadyExists_409(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	doJSON(t, ts, "POST", "/api/init/x", map[string]any{"dataType": "long"})
	code, body := doJSON(t, ts, "POST", "/api/init/x", map[string]any{"dataType": "long"})
	if code != 409 {
		t.Errorf("status %d, want 409", code)
	}
	if body["error"] != "Entity already exists" {
		t.Errorf("error=%v", body["error"])
	}
}

func TestReset_snapsValue(t *testing.T) {
	ts := newTestServer(t, 2, 3)
	defer ts.Close()
	get(t, ts, "/api/next-id/x?dataType=long") // bind as long

	code, body := doJSON(t, ts, "PUT", "/api/reset/x", map[string]any{"value": 5000})
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	reset := body["reset"].(map[string]any)
	// 5000 mod 3 = 2 → no snap.
	if reset["actualValue"] != "5000" {
		t.Errorf("actualValue=%v, want 5000", reset["actualValue"])
	}

	// Now next emission should be 5000.
	_, b := get(t, ts, "/api/next-id/x?dataType=long")
	if b["value"] != "5000" {
		t.Errorf("after reset, value=%v, want 5000", b["value"])
	}
}

func TestReset_notNumeric_400(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	get(t, ts, "/api/next-id/x?dataType=snowflake")
	code, _ := doJSON(t, ts, "PUT", "/api/reset/x", map[string]any{"value": 5})
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
}

func TestReset_unknownEntity_404(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, _ := doJSON(t, ts, "PUT", "/api/reset/missing", map[string]any{"value": 5})
	if code != 404 {
		t.Errorf("status %d, want 404", code)
	}
}

func TestStatus_numericAndSnowflake(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	get(t, ts, "/api/next-id/n?dataType=long")
	get(t, ts, "/api/next-id/n?dataType=long")
	get(t, ts, "/api/next-id/s?dataType=snowflake")

	_, b1 := get(t, ts, "/api/status/n")
	if b1["currentValue"] != "2" || b1["nextValue"] != "3" {
		t.Errorf("numeric status: current=%v next=%v", b1["currentValue"], b1["nextValue"])
	}

	_, b2 := get(t, ts, "/api/status/s")
	cv, _ := b2["currentValue"].(string)
	if !strings.Contains(cv, "Snowflake") {
		t.Errorf("snowflake status currentValue=%v", b2["currentValue"])
	}
}

func TestStatus_unknown_404(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, _ := get(t, ts, "/api/status/missing")
	if code != 404 {
		t.Errorf("status %d, want 404", code)
	}
}

// ---------- list ----------

func TestList(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	get(t, ts, "/api/next-id/a?dataType=long")
	get(t, ts, "/api/next-id/b?dataType=uuid12")
	get(t, ts, "/api/next-id/c?dataType=snowflake")

	code, body := get(t, ts, "/api/list")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	entities, _ := body["entities"].([]any)
	if len(entities) != 3 {
		t.Errorf("len=%d, want 3", len(entities))
	}
	if _, ok := body["shardInfo"].(map[string]any); !ok {
		t.Error("shardInfo missing")
	}
}

// ---------- segment-state ----------

func TestSegmentState_numeric(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	get(t, ts, "/api/next-id/x?dataType=long")
	get(t, ts, "/api/next-id/x?dataType=long")
	get(t, ts, "/api/next-id/x?dataType=long")

	code, body := get(t, ts, "/api/segment-state/x")
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	seg, _ := body["segment"].(map[string]any)
	if seg["size"].(float64) != 10 {
		t.Errorf("size=%v, want 10", seg["size"])
	}
	if seg["cursor"].(float64) != 3 {
		t.Errorf("cursor=%v, want 3", seg["cursor"])
	}
	if seg["remaining"].(float64) != 7 {
		t.Errorf("remaining=%v, want 7", seg["remaining"])
	}
}

func TestSegmentState_nonNumeric_400(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	get(t, ts, "/api/next-id/u?dataType=uuid12")
	code, _ := get(t, ts, "/api/segment-state/u")
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
}

// ---------- parse-snowflake ----------

func TestParseSnowflake_roundTrip(t *testing.T) {
	ts := newTestServer(t, 3, 5)
	defer ts.Close()
	_, b := get(t, ts, "/api/next-id/e?dataType=snowflake")
	id := b["value"].(string)

	code, body := get(t, ts, "/api/parse-snowflake/"+id)
	if code != 200 {
		t.Fatalf("status %d body=%v", code, body)
	}
	if body["shardId"].(float64) != 3 {
		t.Errorf("parsed shardId=%v, want 3", body["shardId"])
	}
	if body["id"] != id {
		t.Errorf("echoed id=%v, want %s", body["id"], id)
	}
}

func TestParseSnowflake_invalid_400(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	code, _ := get(t, ts, "/api/parse-snowflake/not-a-number")
	if code != 400 {
		t.Errorf("status %d, want 400", code)
	}
}

// ---------- method enforcement ----------

func TestMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	// /health only accepts GET — POST should give 405.
	req, _ := http.NewRequest("POST", ts.URL+"/health", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status %d, want 405", resp.StatusCode)
	}
}

// ---------- 404 ----------

func TestUnknownPath_404(t *testing.T) {
	ts := newTestServer(t, 1, 1)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/does-not-exist")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
}
