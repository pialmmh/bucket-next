package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/telcobright/bucket-next/internal/allocator"
	"github.com/telcobright/bucket-next/internal/datatype"
	"github.com/telcobright/bucket-next/internal/shortid"
	"github.com/telcobright/bucket-next/internal/state"
)

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

// formatNumeric returns the on-wire form for an int/long value.
func formatNumeric(dt datatype.DataType, v int64) any {
	if dt == datatype.Long {
		return strconv.FormatInt(v, 10)
	}
	return v
}

// ---------- handlers ----------

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

func (s *Server) handleShardInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"shardId":     s.cfg.ShardID,
		"totalShards": s.cfg.TotalShards,
		"status":      "active",
	})
}

func (s *Server) handleTypes(w http.ResponseWriter, r *http.Request) {
	k, N := s.cfg.ShardID, s.cfg.TotalShards
	desc := map[string]string{
		"int":       fmt.Sprintf("Sequential 32-bit integer (shard %d: %d, %d, %d, ...)", k, k, k+N, k+2*N),
		"long":      fmt.Sprintf("Sequential 64-bit integer (shard %d: %d, %d, %d, ...)", k, k, k+N, k+2*N),
		"snowflake": "64-bit time-ordered unique ID (timestamp + shard + sequence)",
		"uuid8":     "Random 8-character alphanumeric string",
		"uuid12":    "Random 12-character alphanumeric string",
		"uuid16":    "Random 16-character alphanumeric string",
		"uuid22":    "Random 22-character alphanumeric string",
	}
	types := make([]string, len(datatype.All))
	for i, t := range datatype.All {
		types[i] = string(t)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"availableTypes": types,
		"description":    desc,
		"shardInfo": map[string]any{
			"shardId":     s.cfg.ShardID,
			"totalShards": s.cfg.TotalShards,
		},
		"snowflakeInfo": s.forge.Info(s.cfg.TotalShards),
	})
}

func (s *Server) handleNextID(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/next-id/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	dtStr := r.URL.Query().Get("dataType")
	dt, err := datatype.Parse(dtStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Valid dataType is required", err.Error())
		return
	}

	rec, ok := s.state.Get(name)
	if !ok {
		rec = &state.Record{EntityName: name, DataType: dt, CurrentIteration: 0, ShardID: s.cfg.ShardID}
		if err := s.state.Put(*rec); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to register entity", err.Error())
			return
		}
	} else if rec.DataType != dt {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":          "Type mismatch",
			"message":        fmt.Sprintf("Entity '%s' is registered as '%s', cannot use '%s'", name, rec.DataType, dt),
			"registeredType": rec.DataType,
		})
		return
	}

	var value any
	switch {
	case dt == datatype.Snowflake:
		id, err := s.forge.Generate()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Snowflake error", err.Error())
			return
		}
		value = id
	case dt.IsUUID():
		id, err := shortid.Generate(dt.UUIDLength())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UUID error", err.Error())
			return
		}
		value = id
	default:
		v, err := s.allocator.NextID(name, dt)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Allocator error", err.Error())
			return
		}
		value = formatNumeric(dt, v)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entityName": name,
		"dataType":   string(dt),
		"value":      value,
		"shard":      s.cfg.ShardID,
	})
}

func (s *Server) handleNextBatch(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/next-batch/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	dtStr := r.URL.Query().Get("dataType")
	dt, err := datatype.Parse(dtStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Valid dataType is required", err.Error())
		return
	}
	bsStr := r.URL.Query().Get("batchSize")
	bs, err := strconv.Atoi(bsStr)
	if err != nil || bs < 1 || bs > 10000 {
		writeError(w, http.StatusBadRequest, "Valid batchSize is required", "batchSize must be in [1, 10000]")
		return
	}

	rec, ok := s.state.Get(name)
	if !ok {
		rec = &state.Record{EntityName: name, DataType: dt, CurrentIteration: 0, ShardID: s.cfg.ShardID}
		if err := s.state.Put(*rec); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to register entity", err.Error())
			return
		}
	} else if rec.DataType != dt {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error":          "Type mismatch",
			"message":        fmt.Sprintf("Entity '%s' is registered as '%s', cannot use '%s'", name, rec.DataType, dt),
			"registeredType": rec.DataType,
		})
		return
	}

	var values []any
	var startVal, endVal any

	switch {
	case dt == datatype.Snowflake:
		ids, err := s.forge.GenerateBatch(bs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Snowflake error", err.Error())
			return
		}
		values = make([]any, len(ids))
		for i, v := range ids {
			values[i] = v
		}
		if len(ids) > 0 {
			startVal = ids[0]
			endVal = ids[len(ids)-1]
		}
	case dt.IsUUID():
		ids, err := shortid.GenerateBatch(dt.UUIDLength(), bs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "UUID error", err.Error())
			return
		}
		values = make([]any, len(ids))
		for i, v := range ids {
			values[i] = v
		}
		if len(ids) > 0 {
			startVal = ids[0]
			endVal = ids[len(ids)-1]
		}
	default:
		nums, err := s.allocator.NextBatch(name, dt, bs)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Allocator error", err.Error())
			return
		}
		values = make([]any, len(nums))
		for i, v := range nums {
			values[i] = formatNumeric(dt, v)
		}
		if len(nums) > 0 {
			startVal = formatNumeric(dt, nums[0])
			endVal = formatNumeric(dt, nums[len(nums)-1])
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"entityName": name,
		"dataType":   string(dt),
		"batchSize":  bs,
		"startValue": startVal,
		"endValue":   endVal,
		"values":     values,
		"shard":      s.cfg.ShardID,
	})
}

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

	if existing, ok := s.state.Get(name); ok {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":   "Entity already exists",
			"message": fmt.Sprintf("Entity '%s' is already registered with type '%s'", name, existing.DataType),
			"currentStatus": map[string]any{
				"dataType":         existing.DataType,
				"currentIteration": existing.CurrentIteration,
				"shard":            s.cfg.ShardID,
			},
		})
		return
	}

	var initialIter int64
	resp := map[string]any{
		"entityName":  name,
		"dataType":    string(dt),
		"shard":       s.cfg.ShardID,
		"initialized": true,
		"message":     fmt.Sprintf("Entity '%s' initialized successfully with type '%s'", name, dt),
	}

	if body.StartValue != nil && dt.IsNumeric() {
		requested := *body.StartValue
		snapped := s.allocator.SnapForward(requested)
		if err := allocator.CheckOverflow(dt, snapped); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid start value", err.Error())
			return
		}
		initialIter = s.allocator.IterFromValue(snapped)
		resp["requestedStartValue"] = requested
		resp["actualStartValue"] = formatNumeric(dt, snapped)
		if snapped != requested {
			resp["adjusted"] = true
			resp["adjustmentReason"] = fmt.Sprintf("Value adjusted to match shard %d pattern", s.cfg.ShardID)
		}
	}

	rec := state.Record{
		EntityName:       name,
		DataType:         dt,
		CurrentIteration: initialIter,
		ShardID:          s.cfg.ShardID,
	}
	if err := s.state.Put(rec); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to register entity", err.Error())
		return
	}

	if dt.IsNumeric() {
		resp["nextValue"] = formatNumeric(dt, s.allocator.ComputeValue(initialIter))
		resp["pattern"] = fmt.Sprintf("%d, %d, %d, ...", s.cfg.ShardID, s.cfg.ShardID+s.cfg.TotalShards, s.cfg.ShardID+2*s.cfg.TotalShards)
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) handleReset(w http.ResponseWriter, r *http.Request) {
	name := extractEntity(r, "/api/reset/")
	if name == "" {
		writeError(w, http.StatusBadRequest, "Entity name is required", "")
		return
	}
	var body struct {
		Value *int64 `json:"value"`
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

	requested := *body.Value
	snapped := s.allocator.SnapForward(requested)
	if err := allocator.CheckOverflow(rec.DataType, snapped); err != nil {
		writeError(w, http.StatusBadRequest, "Invalid value", err.Error())
		return
	}
	newIter := s.allocator.IterFromValue(snapped)
	prevIter := rec.CurrentIteration

	var prevVal any
	if prevIter > 0 {
		prevVal = formatNumeric(rec.DataType, s.allocator.ComputeValue(prevIter-1))
	}

	rec.CurrentIteration = newIter
	if err := s.state.Put(*rec); err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to reset", err.Error())
		return
	}
	// Drop any in-RAM segment so the next emission re-reserves from the new mark.
	s.allocator.Invalidate(name)

	resp := map[string]any{
		"entityName": name,
		"dataType":   string(rec.DataType),
		"shard":      s.cfg.ShardID,
		"reset": map[string]any{
			"previousValue":      prevVal,
			"requestedValue":     formatNumeric(rec.DataType, requested),
			"actualValue":        formatNumeric(rec.DataType, snapped),
			"previousIteration":  prevIter,
			"newIteration":       newIter,
		},
		"nextValue": formatNumeric(rec.DataType, s.allocator.ComputeValue(newIter)),
		"message":   fmt.Sprintf("Counter reset successfully. Next value will be %s", formatStr(rec.DataType, snapped)),
	}
	if snapped != requested {
		resp["adjusted"] = true
		resp["adjustmentReason"] = fmt.Sprintf("Value adjusted from %d to %d to match shard %d pattern", requested, snapped, s.cfg.ShardID)
	}
	writeJSON(w, http.StatusOK, resp)
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

	resp := map[string]any{
		"entityName":       name,
		"dataType":         string(rec.DataType),
		"currentIteration": rec.CurrentIteration,
		"shard":            s.cfg.ShardID,
	}

	switch {
	case rec.DataType == datatype.Snowflake:
		resp["currentValue"] = "N/A (Snowflake IDs are not sequential)"
		resp["nextValue"] = nil
	case rec.DataType.IsUUID():
		resp["currentValue"] = nil
		resp["nextValue"] = nil
	default:
		if v, ok := s.allocator.CurrentValue(name); ok {
			resp["currentValue"] = formatNumeric(rec.DataType, v)
		} else {
			resp["currentValue"] = nil
		}
		if v, ok := s.allocator.NextValue(name); ok {
			resp["nextValue"] = formatNumeric(rec.DataType, v)
		} else {
			resp["nextValue"] = nil
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	records := s.state.All()
	entities := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		item := map[string]any{
			"entityName":       rec.EntityName,
			"dataType":         string(rec.DataType),
			"currentIteration": rec.CurrentIteration,
			"shard":            s.cfg.ShardID,
			"currentValue":     nil,
		}
		if rec.DataType.IsNumeric() {
			if v, ok := s.allocator.CurrentValue(rec.EntityName); ok {
				item["currentValue"] = formatNumeric(rec.DataType, v)
			}
		} else if rec.DataType == datatype.Snowflake {
			item["currentValue"] = "N/A (Snowflake IDs)"
		}
		entities = append(entities, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entities": entities,
		"shardInfo": map[string]any{
			"shardId":     s.cfg.ShardID,
			"totalShards": s.cfg.TotalShards,
		},
	})
}

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
	if !rec.DataType.IsNumeric() {
		writeError(w, http.StatusBadRequest, "Invalid type",
			fmt.Sprintf("Segment cache only exists for numeric types; entity '%s' is '%s'", name, rec.DataType))
		return
	}
	snap, err := s.allocator.Snapshot(name)
	if err != nil {
		// No segment in RAM yet — return a minimal snapshot from disk.
		writeJSON(w, http.StatusOK, map[string]any{
			"entityName": name,
			"dataType":   string(rec.DataType),
			"segment": map[string]any{
				"size":              s.cfg.SegmentSize,
				"cursor":            0,
				"remaining":         0,
				"watermark":         s.cfg.SegmentRefillWatermark,
				"refillInFlight":    false,
				"diskHighWaterMark": formatNumeric(rec.DataType, s.allocator.ComputeValue(rec.CurrentIteration)),
			},
			"shard": s.cfg.ShardID,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"entityName": name,
		"dataType":   string(rec.DataType),
		"segment":    snap,
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

// formatStr renders a numeric value as a string in the same form the JSON encoder uses
// for long (decimal string) vs int (decimal). Used in human messages.
func formatStr(dt datatype.DataType, v int64) string {
	return strconv.FormatInt(v, 10)
}
