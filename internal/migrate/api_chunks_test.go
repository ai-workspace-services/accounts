package migrate

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestMigrationChunkManifestIntegrity(t *testing.T) {
	tables, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.Repeat("a", 64)
	now := time.Now().UTC()
	manifest := MigrationManifest{Format: 1, Scope: MigrationAPIScope, SnapshotID: uuid.NewString(), ToolSHA256: hash, SourceIdentitySHA256: hash, SourceSchemaSHA256: hash, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Authorization: MigrationAuthorization{SourceAudience: "source", TargetAudience: "target", JobID: uuid.NewString(), SubjectSHA256: hash}, Tables: map[string]BusinessEquality{}, Columns: map[string][]businessColumn{}}
	for _, name := range order {
		d := businessDigest{}
		manifest.Tables[name], err = d.finish()
		if err != nil {
			t.Fatal(err)
		}
		manifest.Columns[name] = tables[name].Columns
	}
	manifest.SnapshotSHA256 = migrationSnapshotHash(manifest)
	if err = validateMigrationChunkManifest(manifest); err != nil {
		t.Fatal(err)
	}
	page := migrationChunkPage{Table: "audit_logs", Rows: []rawRow{{"fixture": json.RawMessage(`1`)}}}
	body, _ := json.Marshal(page)
	chunk := MigrationChunk{Table: page.Table, Rows: 1, Bytes: len(body), SHA256: migrationDigest(page)}
	chunk.ID = migrationChunkID(manifest.SnapshotID, chunk)
	if err = validateMigrationChunk(manifest.SnapshotID, chunk, page); err != nil {
		t.Fatal(err)
	}
	changed := page
	changed.Ordinal = 1
	if validateMigrationChunk(manifest.SnapshotID, chunk, changed) == nil {
		t.Fatal("reordered page accepted")
	}
	manifest.Chunks = []MigrationChunk{chunk}
	fingerprint := manifest.Tables["audit_logs"]
	fingerprint.Rows = 1
	manifest.Tables["audit_logs"] = fingerprint
	manifest.SnapshotSHA256 = migrationSnapshotHash(manifest)
	if err = validateMigrationChunkManifest(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Chunks = append(manifest.Chunks, chunk)
	manifest.SnapshotSHA256 = migrationSnapshotHash(manifest)
	if validateMigrationChunkManifest(manifest) == nil {
		t.Fatal("duplicate/reordered chunk accepted")
	}
	manifest.Chunks = manifest.Chunks[:1]
	manifest.SourceIdentitySHA256 = ""
	manifest.SnapshotSHA256 = migrationSnapshotHash(manifest)
	if validateMigrationChunkManifest(manifest) == nil {
		t.Fatal("missing physical provenance accepted")
	}
}
