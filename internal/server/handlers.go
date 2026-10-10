package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/state"
	"github.com/telcobright/bucket-next/internal/uuidgen"
)

// MaxBatchSize caps one vector request.
const MaxBatchSize = 10000

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, errCode, message string) {
	out := map[string]any{"error": errCode}
	if message != "" {
		out["message"] = message
	}
	writeJSON(w, status, out)
}

// extractEntity strips the prefix from r.URL.Path. Returns empty if missing.
func extractEntity(r *http.Request, prefix string) string {
	return strings.TrimPrefix(r.URL.Path, prefix)
}

// formatNumeric returns the on-wire form for an int/long value: long is a decimal string so
// JavaScript clients do not lose precision above 2^53.
func formatNumeric(dt datatype.DataType, v int64) any {
	if dt == datatype.Long {
		return strconv.FormatInt(v, 10)
	}
	return v
}

// rangeInfo describes a vector of int/long IDs compactly: value i is first + i*step.
type rangeInfo struct {
	First any   `json:"first"`
	Last  any   `json:"last"`
	Step  int64 `json:"step"`
	Count int   `json:"count"`
}

// registerOrCheck auto-registers the entity with dt, or answers 400 if it is bound to
// another type. Returns false when it has written a response.
func (s *Server) registerOrCheck(w http.ResponseWriter, name string, dt datatype.DataType) bool {
	rec, _, err := s.state.Register(name, dt, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to register entity", err.Error())
		return false
	}
	if rec.DataType != dt {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":          "Type mismatch",
			"message":        fmt.Sprintf("Entity '%s' is registered as '%s', cannot use '%s'", name, rec.DataType, dt),
			"registeredType": rec.DataType,
		})
		return false
	}
	return true
}

// mint produces n new IDs for the entity in wire form. It is the one code path behind both
// the scalar (n=1) and the vector API, for every type. For int/long it also returns the
// vector as a range: the n values are consecutive on this shard.
func (s *Server) mint(name string, dt datatype.DataType, n int) ([]any, *rangeInfo, error) {
	values := make([]any, n)
	if dt == datatype.Snowflake {
		ids, err := s.forge.GenerateBatch(n)
		if err != nil {
			return nil, nil, err
		}
		for i, id := range ids {
			values[i] = id
		}
		return values, nil, nil
	}
	first, err := s.allocator.Next(name, dt, int64(n))
	if err != nil {
		return nil, nil, err
	}
	layout := s.allocator.Layout()
	tweak := uuidgen.TweakFor(name)
	for i := 0; i < n; i++ {
		v := layout.Value(first + int64(i))
		if dt.IsUUID() {
			str, err := s.codec.Encode(v, dt.UUIDLength(), tweak)
			if err != nil {
				return nil, nil, err
			}
			values[i] = str
		} else {
			values[i] = formatNumeric(dt, v)
		}
	}
	if !dt.IsNumeric() {
		return values, nil, nil
	}
	return values, &rangeInfo{First: values[0], Last: values[n-1], Step: layout.TotalShards, Count: n}, nil
}

func writeMintError(w http.ResponseWriter, name string, dt datatype.DataType, err error) {
	if errors.Is(err, allocator.ErrExhausted) {
		writeError(w, http.StatusConflict, "Range exhausted",
			fmt.Sprintf("Entity '%s' has no %s values left on this shard (max %d)", name, dt, dt.MaxValue()))
		return
	}
	writeError(w, http.StatusInternalServerError, "Generation failed", err.Error())
}

// parseCounterValue validates a user-supplied value for an int/long entity and snaps it to
// this shard. It returns the snapped value, or writes a 400 and returns ok=false.
func (s *Server) parseCounterValue(w http.ResponseWriter, dt datatype.DataType, v int64, field string) (int64, bool) {
	max := dt.MaxValue()
	if v < 1 || v > max {
		writeError(w, http.StatusBadRequest, "Invalid "+field,
			fmt.Sprintf("%s must be between 1 and %d for type %s", field, max, dt))
		return 0, false
	}
	snapped := s.allocator.Layout().SnapForward(v)
	if snapped < v || snapped > max {
		writeError(w, http.StatusBadRequest, "Invalid "+field,
			fmt.Sprintf("%s %d has no value for shard %d at or below %d", field, v, s.cfg.ShardID, max))
		return 0, false
	}
	return snapped, true
}

// ---------- health / introspection ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	gen, col, wait := s.forge.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":      "healthy",
		"uptime":      time.Since(s.startedAt).Seconds(),
		"shard":       s.cfg.ShardID,
		"totalShards": s.cfg.TotalShards,
		"snowflakeStats": map[string]any{
			"generated":   gen,
			"collisions":  col,
			"waits":       wait,
			"shardId":     s.cfg.ShardID,
			"totalShards": s.cfg.TotalShards,
			"maxIdsPerMs": 4096,
			"maxShards":   1024,
		},
	})
}

// handleShardInfo also reports the values every shard of a cluster must share, so an
// operator can compare them across nodes.
func (s *Server) handleShardInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"shardId":            s.cfg.ShardID,
		"totalShards":        s.cfg.TotalShards,
		"status":             "active",
		"uuidKeyFingerprint": s.codec.Fingerprint(),
		"snowflakeEpochMs":   s.cfg.SnowflakeEpochMs,
	})
}

func (s *Server) handleTypes(w http.ResponseWriter, r *http.Request) {
	k, N := s.cfg.ShardID, s.cfg.TotalShards
	uuidDesc := func(n int) string {
		return fmt.Sprintf("Unique %d-character alphanumeric string (shard-interleaved counter, keyed scramble)", n)
	}
	desc := map[string]string{
		"int":       fmt.Sprintf("Sequential 32-bit integer (shard %d: %d, %d, %d, ...)", k, k, k+N, k+2*N),
		"long":      fmt.Sprintf("Sequential 64-bit integer (shard %d: %d, %d, %d, ...)", k, k, k+N, k+2*N),
		"snowflake": "64-bit time-ordered unique ID (timestamp + shard + sequence)",
		"uuid8":     uuidDesc(8),
		"uuid12":    uuidDesc(12),
		"uuid16":    uuidDesc(16),
		"uuid22":    uuidDesc(22),
	}
	types := make([]string, len(datatype.All))
	for i, t := range datatype.All {
		types[i] = string(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"availableTypes": types,
		"description":    desc,
		"apiVersions": map[string]string{
			"scalar": "GET /api/next-id/{entity}?dataType=T — one ID",
			"vector": fmt.Sprintf("GET /api/next-batch/{entity}?dataType=T&batchSize=N — N IDs (1..%d)", MaxBatchSize),
		},
		"shardInfo": map[string]any{
			"shardId":     s.cfg.ShardID,
			"totalShards": s.cfg.TotalShards,
		},
		"snowflakeInfo": s.forge.Info(s.cfg.TotalShards),
	})
}

// ---------- ID generation: scalar + vector ----------

// handleNextID is the scalar version: one new ID.
func (s *Server) handleNextID(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/next-id/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	dt, err := datatype.Parse(r.URL.Query().Get("dataType"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Valid dataType is required", err.Error())
		return
	}
	if !s.registerOrCheck(w, name, dt) {
		return
	}
	values, _, err := s.mint(name, dt, 1)
	if err != nil {
		writeMintError(w, name, dt, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entityName": name,
		"dataType":   string(dt),
		"value":      values[0],
		"shard":      s.cfg.ShardID,
	})
}

// handleNextBatch is the vector version: a range of new IDs.
func (s *Server) handleNextBatch(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/next-batch/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	dt, err := datatype.Parse(r.URL.Query().Get("dataType"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Valid dataType is required", err.Error())
		return
	}
	bs, err := strconv.Atoi(r.URL.Query().Get("batchSize"))
	if err != nil || bs < 1 || bs > MaxBatchSize {
		writeError(w, http.StatusBadRequest, "Valid batchSize is required",
			fmt.Sprintf("batchSize must be in [1, %d]", MaxBatchSize))
		return
	}
	if !s.registerOrCheck(w, name, dt) {
		return
	}
	values, rng, err := s.mint(name, dt, bs)
	if err != nil {
		writeMintError(w, name, dt, err)
		return
	}
	resp := map[string]any{
		"entityName": name,
		"dataType":   string(dt),
		"batchSize":  bs,
		"startValue": values[0],
		"endValue":   values[bs-1],
		"values":     values,
		"shard":      s.cfg.ShardID,
	}
	if rng != nil {
		resp["range"] = rng
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---------- entity lifecycle ----------

func (s *Server) handleInit(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/init/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	var body struct {
		DataType   string `json:"dataType"`
		StartValue *int64 `json:"startValue"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body", err.Error())
		return
	}
	dt, err := datatype.Parse(body.DataType)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Valid dataType is required", err.Error())
		return
	}
	if body.StartValue != nil && !dt.IsNumeric() {
		writeError(w, http.StatusBadRequest, "Invalid start value",
			fmt.Sprintf("startValue is only supported for int and long, not %s", dt))
		return
	}

	layout := s.allocator.Layout()
	var initialIter int64
	resp := map[string]any{
		"entityName":  name,
		"dataType":    string(dt),
		"shard":       s.cfg.ShardID,
		"initialized": true,
		"message":     fmt.Sprintf("Entity '%s' initialized successfully with type '%s'", name, dt),
	}
	if body.StartValue != nil {
		requested := *body.StartValue
		snapped, ok := s.parseCounterValue(w, dt, requested, "startValue")
		if !ok {
			return
		}
		initialIter = layout.Iter(snapped)
		resp["requestedStartValue"] = requested
		resp["actualStartValue"] = formatNumeric(dt, snapped)
		if snapped != requested {
			resp["adjusted"] = true
			resp["adjustmentReason"] = fmt.Sprintf("Value adjusted to match shard %d pattern", s.cfg.ShardID)
		}
	}

	rec, created, err := s.state.Register(name, dt, initialIter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to register entity", err.Error())
		return
	}
	if !created {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "Entity already exists",
			"message": fmt.Sprintf("Entity '%s' is already registered with type '%s'", name, rec.DataType),
			"currentStatus": map[string]any{
				"dataType":         rec.DataType,
				"currentIteration": rec.CurrentIteration,
				"shard":            s.cfg.ShardID,
			},
		})
		return
	}
	if dt.IsNumeric() {
		resp["nextValue"] = formatNumeric(dt, layout.Value(initialIter))
		resp["pattern"] = fmt.Sprintf("%d, %d, %d, ...", s.cfg.ShardID, s.cfg.ShardID+s.cfg.TotalShards, s.cfg.ShardID+2*s.cfg.TotalShards)
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleReset moves an int/long counter. Moving it below IDs that may already be issued is
// refused unless the body says "force": true.
func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/reset/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	var body struct {
		Value *int64 `json:"value"`
		Force bool   `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid JSON body", err.Error())
		return
	}
	if body.Value == nil {
		writeError(w, http.StatusBadRequest, "value is required", "")
		return
	}
	rec, ok := s.state.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "Entity not found",
			fmt.Sprintf("Entity '%s' is not registered", name))
		return
	}
	if !rec.DataType.IsNumeric() {
		writeError(w, http.StatusBadRequest, "Invalid operation",
			fmt.Sprintf("Cannot reset counter for type '%s'. Reset is only available for int and long.", rec.DataType))
		return
	}
	dt := rec.DataType
	requested := *body.Value
	snapped, ok := s.parseCounterValue(w, dt, requested, "value")
	if !ok {
		return
	}
	layout := s.allocator.Layout()
	newIter := layout.Iter(snapped)
	prevIter, err := s.allocator.Reset(name, newIter, body.Force)
	if errors.Is(err, allocator.ErrBackward) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "Backward reset refused",
			"message": fmt.Sprintf("Next value is %d; moving to %d could re-issue IDs that are already in use. "+
				"Send \"force\": true to do it anyway.", layout.Value(prevIter), snapped),
			"nextValue": formatNumeric(dt, layout.Value(prevIter)),
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset", err.Error())
		return
	}

	resp := map[string]any{
		"entityName": name,
		"dataType":   string(dt),
		"shard":      s.cfg.ShardID,
		"reset": map[string]any{
			"previousNextValue": formatNumeric(dt, layout.Value(prevIter)),
			"requestedValue":    formatNumeric(dt, requested),
			"actualValue":       formatNumeric(dt, snapped),
			"previousIteration": prevIter,
			"newIteration":      newIter,
			"forced":            body.Force && newIter < prevIter,
		},
		"nextValue": formatNumeric(dt, snapped),
		"message":   fmt.Sprintf("Counter reset successfully. Next value will be %d", snapped),
	}
	if snapped != requested {
		resp["adjusted"] = true
		resp["adjustmentReason"] = fmt.Sprintf("Value adjusted from %d to %d to match shard %d pattern", requested, snapped, s.cfg.ShardID)
	}
	writeJSON(w, http.StatusOK, resp)
}

// entityView is the status/list shape. For int/long, currentValue is the last value this
// process handed out and nextValue the one it will hand out next. uuid values are opaque,
// so only the counter position is shown.
func (s *Server) entityView(rec state.Record) map[string]any {
	view := map[string]any{
		"entityName":       rec.EntityName,
		"dataType":         string(rec.DataType),
		"currentIteration": rec.CurrentIteration,
		"shard":            s.cfg.ShardID,
		"currentValue":     nil,
		"nextValue":        nil,
	}
	switch {
	case rec.DataType == datatype.Snowflake:
		view["currentValue"] = "N/A (Snowflake IDs are not sequential)"
	case rec.DataType.IsNumeric():
		snap, err := s.allocator.Snapshot(rec.EntityName)
		if err != nil {
			return view
		}
		layout := s.allocator.Layout()
		if snap.Loaded && snap.Issued > 0 {
			view["currentValue"] = formatNumeric(rec.DataType, layout.Value(snap.NextIteration-1))
		}
		view["nextValue"] = formatNumeric(rec.DataType, layout.Value(snap.NextIteration))
	}
	return view
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/status/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	rec, ok := s.state.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "Entity not found", "")
		return
	}
	writeJSON(w, http.StatusOK, s.entityView(rec))
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	records := s.state.All()
	entities := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		entities = append(entities, s.entityView(rec))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entities": entities,
		"shardInfo": map[string]any{
			"shardId":     s.cfg.ShardID,
			"totalShards": s.cfg.TotalShards,
		},
	})
}

// handleSegmentState shows the in-RAM reservation of a counter entity (int, long, uuid*).
func (s *Server) handleSegmentState(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/segment-state/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	rec, ok := s.state.Get(name)
	if !ok {
		writeError(w, http.StatusNotFound, "Entity not found", "")
		return
	}
	if !rec.DataType.IsCounter() {
		writeError(w, http.StatusBadRequest, "Invalid type",
			fmt.Sprintf("Reservations exist only for counter types (int, long, uuid*); entity '%s' is '%s'", name, rec.DataType))
		return
	}
	snap, err := s.allocator.Snapshot(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Snapshot failed", err.Error())
		return
	}
	seg := map[string]any{
		"size":           s.cfg.SegmentSize,
		"watermark":      s.cfg.SegmentRefillWatermark,
		"loaded":         snap.Loaded,
		"issued":         snap.Issued,
		"remaining":      snap.Remaining,
		"refillInFlight": snap.RefillInFlight,
		"nextIteration":  snap.NextIteration,
		"reservedEnd":    snap.ReservedEnd,
		"diskMark":       snap.DiskMark,
	}
	if rec.DataType.IsNumeric() {
		layout := s.allocator.Layout()
		seg["nextValue"] = formatNumeric(rec.DataType, layout.Value(snap.NextIteration))
		seg["diskHighWaterMark"] = formatNumeric(rec.DataType, layout.Value(snap.DiskMark))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entityName": name,
		"dataType":   string(rec.DataType),
		"segment":    seg,
		"shard":      s.cfg.ShardID,
	})
}

func (s *Server) handleParseSnowflake(w http.ResponseWriter, r *http.Request) {
	idStr := extractEntity(r, "/api/parse-snowflake/")
	if idStr == "" {
		writeError(w, http.StatusBadRequest, "Snowflake id is required", "")
		return
	}
	parsed, err := s.forge.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid Snowflake ID", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, parsed)
}
