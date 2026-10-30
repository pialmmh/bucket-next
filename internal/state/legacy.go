package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// legacyFile is a pre-tenant state file: format 1 (a bare JSON array of records) or format 2
// (one object per shard: identity, Snowflake mark, records). Neither knows tenants.
type legacyFile struct {
	Path    string
	Format  int
	SfMark  int64
	Records []Record
}

type fileV2 struct {
	Version int `json:"version"`
	Identity
	SnowflakeMarkMs int64    `json:"snowflakeMarkMs"`
	Records         []Record `json:"records"`
}

// readLegacy loads a pre-tenant state file and checks it belongs to id. It returns nil, nil
// when the file does not exist. It never writes.
func readLegacy(path string, id Identity) (*legacyFile, error) {
	b, err := readNonEmpty(path)
	if b == nil || err != nil {
		return nil, err
	}
	lf := &legacyFile{Path: path}
	if b[0] == '[' {
		if err := json.Unmarshal(b, &lf.Records); err != nil {
			return nil, fmt.Errorf("parse state %s: %w", path, err)
		}
		lf.Format = 1
	} else {
		var f fileV2
		if err := json.Unmarshal(b, &f); err != nil {
			return nil, fmt.Errorf("parse state %s: %w", path, err)
		}
		if f.Version != 2 {
			return nil, fmt.Errorf("state file %s has version %d; a pre-tenant state file is format 1 or 2", path, f.Version)
		}
		if err := checkIdentity(path, f.Identity, id); err != nil {
			return nil, err
		}
		lf.Format, lf.SfMark, lf.Records = 2, f.SnowflakeMarkMs, f.Records
	}
	if err := checkRecords(path, lf.Records, id, false); err != nil {
		return nil, err
	}
	return lf, nil
}

// mergeLegacy carries a pre-tenant file's records into a tenant's file. Marks only move
// forward: an entity in both keeps the higher mark, so merging the same file twice, or a file
// older than the tenant's, changes nothing. It returns how many entities it added or moved.
func mergeLegacy(tf *tenantFile, lf *legacyFile, shardID int) (int, error) {
	byName := make(map[string]Record, len(tf.Records)+len(lf.Records))
	for _, r := range tf.Records {
		byName[r.EntityName] = r
	}
	changed := 0
	for _, r := range lf.Records {
		r.ShardID = shardID
		if r.DataType.IsUUID() {
			r.UUIDTweak = TweakEntity
		}
		cur, ok := byName[r.EntityName]
		switch {
		case !ok:
			byName[r.EntityName] = r
			changed++
		case cur.DataType != r.DataType:
			return 0, fmt.Errorf("entity %q is %s in %s but %s in tenant %s's file — refusing to merge two counters of different types",
				r.EntityName, r.DataType, lf.Path, cur.DataType, tf.Tenant)
		case cur.UUIDTweak != r.UUIDTweak:
			return 0, fmt.Errorf("uuid entity %q was minted with two scrambles (%s in %s, %s in tenant %s's file): "+
				"its strings could repeat — refusing to merge", r.EntityName, r.UUIDTweak, lf.Path, cur.UUIDTweak, tf.Tenant)
		case r.CurrentIteration > cur.CurrentIteration:
			cur.CurrentIteration = r.CurrentIteration
			byName[r.EntityName] = cur
			changed++
		}
	}
	tf.Records = make([]Record, 0, len(byName))
	for _, r := range byName {
		tf.Records = append(tf.Records, r)
	}
	sortRecords(tf.Records)
	return changed, nil
}

// retireLegacy renames a merged state.json out of the way: state.json.migrated-v<N>, or with a
// time suffix when that name is taken. Its marks live in the tenant's file from now on.
func retireLegacy(dir string, format int) (string, error) {
	src := filepath.Join(dir, LegacyFile)
	dst := fmt.Sprintf("%s.migrated-v%d", src, format)
	if _, err := os.Stat(dst); err == nil {
		dst = fmt.Sprintf("%s.%d", dst, time.Now().UnixNano())
	}
	if err := os.Rename(src, dst); err != nil {
		return "", fmt.Errorf("retire %s: %w", src, err)
	}
	return dst, syncDir(dir)
}

// ExportV2 is the route back to a pre-tenant binary. It writes tenant's records with the
// CURRENT marks as a format-2 state.json, then retires shard.json, so the old binary carries
// on from where this one stopped and a later start of this binary carries state.json over
// again (marks only move forward). Hold the layout's lock (the service must be stopped).
//
// It refuses when another tenant has entities (a pre-tenant binary serves ONE tenant; the
// others would be stranded) and when a uuid entity has the tenant's scramble (the old binary
// would continue it with another scramble).
func (l *Layout) ExportV2(tenant string) (string, error) {
	st, ok := l.Tenants[tenant]
	if !ok {
		return "", fmt.Errorf("tenant %q is not served by this config", tenant)
	}
	others, err := tenantFilesOnDisk(l.Dir)
	if err != nil {
		return "", err
	}
	for _, t := range others {
		if t == tenant {
			continue
		}
		f, err := readTenantFile(tenantPath(l.Dir, t), t, l.Shard.Identity())
		if err != nil {
			return "", err
		}
		if f != nil && len(f.Records) > 0 {
			return "", fmt.Errorf("tenant %s has %d entities: a pre-tenant binary serves one tenant and would strand them — refused", t, len(f.Records))
		}
	}
	records := st.All()
	for _, r := range records {
		if r.DataType.IsUUID() && r.UUIDTweak != TweakEntity {
			return "", fmt.Errorf("uuid entity %q has the tenant's scramble: a pre-tenant binary would continue it with another one "+
				"and could repeat its strings — refused", r.EntityName)
		}
	}
	legacyPath := filepath.Join(l.Dir, LegacyFile)
	if _, err := os.Stat(legacyPath); err == nil {
		return "", fmt.Errorf("%s already exists — refusing to overwrite it", legacyPath)
	}
	for i := range records {
		records[i].UUIDTweak = ""
	}
	if err := writeJSONDurable(legacyPath, fileV2{
		Version:         2,
		Identity:        l.Shard.Identity(),
		SnowflakeMarkMs: l.Shard.SnowflakeMark(),
		Records:         records,
	}); err != nil {
		return "", err
	}
	shardPath := filepath.Join(l.Dir, ShardFile)
	retired := fmt.Sprintf("%s.exported-%d", shardPath, time.Now().UnixNano())
	if err := os.Rename(shardPath, retired); err != nil {
		return "", fmt.Errorf("retire %s: %w", shardPath, err)
	}
	return legacyPath, syncDir(l.Dir)
}
