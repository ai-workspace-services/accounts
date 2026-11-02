package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"
)

// CatchupPreview is read-only evidence, not an apply authorization.
type CatchupPreview struct {
	BaselinePlanID       string                         `json:"baseline_plan_id"`
	BaselinePlanSHA256   string                         `json:"baseline_plan_sha256"`
	SourceSnapshotID     string                         `json:"source_snapshot_id"`
	SourceSnapshotSHA256 string                         `json:"source_snapshot_sha256"`
	TargetIdentitySHA256 string                         `json:"target_identity_sha256"`
	TargetSchemaSHA256   string                         `json:"target_schema_sha256"`
	TargetBaselineSHA256 string                         `json:"target_baseline_sha256"`
	Tables               map[string]CatchupTableSummary `json:"tables"`
}

func catchupTargetLeaves(ctx context.Context, tx *sql.Tx, name string, table businessTable) ([]businessLeaf, error) {
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(t)::text FROM public.`+quoteBusiness(name)+` t`)
	if err != nil {
		return nil, errors.New("catchup target scan failed")
	}
	defer rows.Close()
	digest := businessDigest{}
	for rows.Next() {
		var body string
		if rows.Scan(&body) != nil || len(body) > 1<<20 {
			return nil, errors.New("catchup target row invalid")
		}
		var row rawRow
		if json.Unmarshal([]byte(body), &row) != nil {
			return nil, errors.New("catchup target JSON invalid")
		}
		if err = digest.add(row, table); err != nil {
			return nil, err
		}
	}
	if rows.Err() != nil {
		return nil, errors.New("catchup target scan interrupted")
	}
	return digest.Leaves, nil
}

// previewCommittedCatchup reads the target in a single repeatable-read
// transaction and never changes business rows. The caller must authorize the
// request and ensure new source chunks are staged under the same subject.
func (s *migrationServer) previewCommittedCatchup(ctx context.Context, baselineID, baselineHash, subject string, manifest MigrationManifest) (CatchupPreview, error) {
	var report CatchupPreview
	old, ok, err := ReadMigrationReceipt(ctx, s.DB, baselineID, baselineHash, subject)
	if err != nil {
		return report, err
	}
	if !ok {
		return report, errors.New("committed baseline required")
	}
	if err = validateMigrationChunkManifest(manifest); err != nil {
		return report, err
	}
	if !s.matchesSourceAuthorization(manifest.Authorization, subject) || manifest.SourceIdentitySHA256 != old.Snapshot.SourceIdentitySHA256 {
		return report, errors.New("catchup source identity mismatch")
	}
	base, err := s.loadCommittedBaseline(ctx, baselineID, baselineHash, subject)
	if err != nil {
		return report, err
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return report, err
	}
	source := map[string]*businessDigest{}
	for _, name := range order {
		source[name] = &businessDigest{}
	}
	for _, chunk := range manifest.Chunks {
		if err = ctx.Err(); err != nil {
			return report, err
		}
		page, e := s.chunkReader(subject)(chunk)
		if e != nil {
			return report, e
		}
		if e = validateMigrationChunk(manifest.SnapshotID, chunk, page); e != nil {
			return report, e
		}
		for _, row := range page.Rows {
			if e = source[chunk.Table].add(row, tables[chunk.Table]); e != nil {
				return report, e
			}
		}
	}
	for _, name := range order {
		check := businessDigest{Leaves: append([]businessLeaf(nil), source[name].Leaves...)}
		got, e := check.finish()
		if e != nil || got != manifest.Tables[name] {
			return report, errors.New("catchup source fingerprint mismatch")
		}
	}
	tx, err := migrationBegin(ctx, s.DB, true)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	identity, err := migrationIdentity(ctx, tx)
	if err != nil || identity != old.TargetIdentitySHA256 || identity == manifest.SourceIdentitySHA256 {
		return report, errors.New("catchup target identity mismatch")
	}
	schema, err := migrationSchema(ctx, tx, order)
	if err != nil {
		return report, err
	}
	targetBaseline, err := migrationStreamingBaseline(ctx, tx, tables, order)
	if err != nil {
		return report, err
	}
	report = CatchupPreview{BaselinePlanID: baselineID, BaselinePlanSHA256: baselineHash, SourceSnapshotID: manifest.SnapshotID, SourceSnapshotSHA256: manifest.SnapshotSHA256, TargetIdentitySHA256: identity, TargetSchemaSHA256: schema, TargetBaselineSHA256: migrationDigest(targetBaseline), Tables: map[string]CatchupTableSummary{}}
	for _, name := range order {
		leaves, e := catchupTargetLeaves(ctx, tx, name, tables[name])
		if e != nil {
			return CatchupPreview{}, e
		}
		summary, e := compareCatchupTable(name, base[name], source[name].Leaves, leaves)
		if e != nil {
			return CatchupPreview{}, e
		}
		report.Tables[name] = summary
	}
	if err = tx.Commit(); err != nil {
		return CatchupPreview{}, errors.New("catchup snapshot finalization failed")
	}
	return report, nil
}

type catchupPreviewRequest struct {
	MigrationPreviewRequest
	BaselinePlanID     string `json:"baseline_plan_id"`
	BaselinePlanSHA256 string `json:"baseline_plan_sha256"`
}
type catchupPreviewStage struct {
	Binding   string         `json:"binding"`
	Report    CatchupPreview `json:"report"`
	ExpiresAt time.Time      `json:"expires_at"`
}

func (s *migrationServer) catchupPreview(w http.ResponseWriter, r *http.Request, subject string) {
	var request catchupPreviewRequest
	if migrationDecode(w, r, &request) != nil || !migrationUUID(request.RequestID) || !migrationUUID(request.SnapshotID) || !migrationUUID(request.BaselinePlanID) || !migrationSHA.MatchString(request.BaselinePlanSHA256) || request.SourceToken == "" || len(request.SourceToken) > 8192 {
		migrationError(w, 400, "invalid_request")
		return
	}
	if _, _, err := s.Peers.client(request.SourceURL); err != nil {
		migrationError(w, 403, "source_peer_denied")
		return
	}
	binding := migrationDigest([]string{request.SourceURL, request.SnapshotID, request.BaselinePlanID, request.BaselinePlanSHA256, migrationDigest(s.authorization(subject))})
	var stage catchupPreviewStage
	err := s.store.locked(r.Context(), func() error {
		e := s.store.read("catchup-preview", request.RequestID, subject, &stage)
		if e == nil {
			if stage.Binding != binding {
				return errors.New("catchup request ID reused")
			}
			return nil
		}
		if !errors.Is(e, os.ErrNotExist) {
			return e
		}
		manifest, e := s.downloadChunks(r.Context(), request.MigrationPreviewRequest, subject)
		if e != nil {
			return e
		}
		report, e := s.previewCommittedCatchup(r.Context(), request.BaselinePlanID, request.BaselinePlanSHA256, subject, manifest)
		if e != nil {
			return e
		}
		stage = catchupPreviewStage{Binding: binding, Report: report, ExpiresAt: manifest.ExpiresAt}
		return s.store.write("catchup-preview", request.RequestID, subject, stage)
	})
	if err != nil {
		migrationError(w, 409, "catchup_preview_unavailable")
		return
	}
	if !time.Now().Before(stage.ExpiresAt) {
		migrationError(w, 410, "catchup_preview_expired")
		return
	}
	migrationJSON(w, 200, stage.Report)
}
