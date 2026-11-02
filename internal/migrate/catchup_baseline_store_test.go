package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestCatchupBaselineEncryptedAndRestartable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	s, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	page := catchupBaselinePage{PlanID: "11111111-1111-4111-8111-111111111111", Table: "users", Leaves: []catchupBaselineLeaf{{KeySHA256: strings.Repeat("a", 64), RowSHA256: strings.Repeat("b", 64)}}}
	ref, err := s.writeCatchupBaseline("subject", page)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.writeCatchupBaseline("subject", page)
	if err != nil || again != ref {
		t.Fatalf("retry failed %v", err)
	}
	encrypted, err := os.ReadFile(s.path("catchup-baseline", ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encrypted, []byte(page.Leaves[0].RowSHA256)) {
		t.Fatal("plaintext digest persisted")
	}
	restarted, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := restarted.readCatchupBaseline("subject", ref)
	if err != nil || len(got.Leaves) != 1 || got.Leaves[0] != page.Leaves[0] {
		t.Fatalf("restart read failed: %v", err)
	}
	if _, err = restarted.readCatchupBaseline("another-subject", ref); err == nil {
		t.Fatal("wrong subject accepted")
	}
	altered := ref
	altered.Ordinal++
	if _, err = restarted.readCatchupBaseline("subject", altered); err == nil {
		t.Fatal("altered reference accepted")
	}
	encrypted[len(encrypted)-1] ^= 1
	if err = os.WriteFile(s.path("catchup-baseline", ref.ID), encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.readCatchupBaseline("subject", ref); err == nil {
		t.Fatal("tampered page accepted")
	}
}
func TestCatchupBaselineRejectsUnorderedLeaves(t *testing.T) {
	p := catchupBaselinePage{PlanID: "11111111-1111-4111-8111-111111111111", Table: "users", Leaves: []catchupBaselineLeaf{{KeySHA256: strings.Repeat("b", 64), RowSHA256: strings.Repeat("c", 64)}, {KeySHA256: strings.Repeat("a", 64), RowSHA256: strings.Repeat("d", 64)}}}
	if _, err := catchupBaselineRefFor(p); err == nil {
		t.Fatal("unsorted page accepted")
	}
}

func TestCommittedBaselineRequiresValidPlanAndIsRestartable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{5}, 32)
	store, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	_, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := MigrationManifest{SnapshotID: "22222222-2222-4222-8222-222222222222", Tables: map[string]BusinessEquality{}}
	for _, table := range order {
		empty := businessDigest{}
		snapshot.Tables[table], err = empty.finish()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot.SnapshotSHA256 = migrationSnapshotHash(snapshot)
	plan := MigrationPlan{PlanID: "11111111-1111-4111-8111-111111111111", State: "preview", Snapshot: snapshot, TargetIdentitySHA256: strings.Repeat("a", 64)}
	plan.PlanSHA256 = migrationPlanHash(plan)
	server := &migrationServer{store: store}
	if err = server.captureCommittedBaseline(context.Background(), plan, "subject"); err == nil {
		t.Fatal("preview published as committed")
	}
	plan.State = "applied"
	if err = server.captureCommittedBaseline(context.Background(), plan, "subject"); err != nil {
		t.Fatal(err)
	}
	reopened, err := newMigrationStore(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	server.store = reopened
	if err = server.captureCommittedBaseline(context.Background(), plan, "subject"); err != nil {
		t.Fatal("restart retry:", err)
	}
	plan.TargetIdentitySHA256 = strings.Repeat("b", 64)
	if err = server.captureCommittedBaseline(context.Background(), plan, "subject"); err == nil {
		t.Fatal("altered plan accepted")
	}
}

func TestCommittedBaselineCapturesValidatedChunk(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := newMigrationStore(dir, bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		t.Fatal(err)
	}
	row := rawRow{}
	for _, col := range tables["users"].Columns {
		row[col.Name] = json.RawMessage("null")
	}
	page := migrationChunkPage{Table: "users", Rows: []rawRow{row}}
	blob, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := MigrationManifest{SnapshotID: "22222222-2222-4222-8222-222222222222", Tables: map[string]BusinessEquality{}}
	chunk := MigrationChunk{Table: "users", Rows: 1, Bytes: len(blob), SHA256: migrationDigest(page)}
	chunk.ID = migrationChunkID(snapshot.SnapshotID, chunk)
	snapshot.Chunks = []MigrationChunk{chunk}
	for _, name := range order {
		digest := businessDigest{}
		if name == "users" {
			if err = digest.add(row, tables[name]); err != nil {
				t.Fatal(err)
			}
		}
		snapshot.Tables[name], err = digest.finish()
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot.SnapshotSHA256 = migrationSnapshotHash(snapshot)
	plan := MigrationPlan{PlanID: "11111111-1111-4111-8111-111111111111", State: "preview", Snapshot: snapshot, TargetIdentitySHA256: strings.Repeat("a", 64)}
	plan.PlanSHA256 = migrationPlanHash(plan)
	plan.State = "applied"
	if err = store.write("target-chunk", chunk.ID, "subject", page); err != nil {
		t.Fatal(err)
	}
	server := &migrationServer{store: store}
	if err = server.captureCommittedBaseline(context.Background(), plan, "subject"); err != nil {
		t.Fatal(err)
	}
	var index catchupBaselineIndex
	if err = store.read("catchup-index", plan.PlanID, "subject", &index); err != nil || len(index.Pages) != 1 {
		t.Fatalf("index missing: %v", err)
	}
	baseline, err := store.readCatchupBaseline("subject", index.Pages[0])
	if err != nil || len(baseline.Leaves) != 1 {
		t.Fatalf("page missing: %v", err)
	}
}
