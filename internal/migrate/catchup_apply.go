package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

type CatchupReceipt struct {
	Report    CatchupPreview `json:"report"`
	AppliedAt time.Time      `json:"applied_at"`
}

func readCatchupReceiptTx(ctx context.Context, tx *sql.Tx, id, hash, subject string) (CatchupReceipt, bool, error) {
	var result CatchupReceipt
	var storedHash, storedSubject string
	var data []byte
	err := tx.QueryRowContext(ctx, `SELECT report_sha256,subject,receipt FROM accounts_migration_api.catchup_receipts WHERE request_id=$1`, id).Scan(&storedHash, &storedSubject, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, errors.New("catchup receipt unavailable")
	}
	if storedHash != hash || storedSubject != subject || json.Unmarshal(data, &result) != nil || result.Report.RequestID != id || result.Report.ReportSHA256 != hash || catchupPreviewHash(result.Report) != hash || result.AppliedAt.IsZero() {
		return CatchupReceipt{}, false, errors.New("catchup receipt binding mismatch")
	}
	identity, e := migrationIdentity(ctx, tx)
	if e != nil || identity != result.Report.TargetIdentitySHA256 {
		return CatchupReceipt{}, false, errors.New("catchup receipt target mismatch")
	}
	return result, true, nil
}

func readCatchupReceipt(ctx context.Context, db *sql.DB, id, hash, subject string) (CatchupReceipt, bool, error) {
	tx, err := migrationBegin(ctx, db, true)
	if err != nil {
		return CatchupReceipt{}, false, err
	}
	defer tx.Rollback()
	return readCatchupReceiptTx(ctx, tx, id, hash, subject)
}

// catchupApply accepts only a previously persisted preview bound to the
// authenticated operator, reviewed hash and staged source manifest.
func (s *migrationServer) catchupApply(w http.ResponseWriter, r *http.Request, id, subject string) {
	var request struct {
		ReportSHA256 string `json:"report_sha256"`
	}
	if migrationDecode(w, r, &request) != nil || !migrationSHA.MatchString(request.ReportSHA256) {
		migrationError(w, 400, "invalid_request")
		return
	}
	var stage catchupPreviewStage
	var transfer migrationTransferIndex
	if s.store.read("catchup-preview", id, subject, &stage) != nil || stage.Report.RequestID != id ||
		stage.Report.ReportSHA256 != request.ReportSHA256 || catchupPreviewHash(stage.Report) != request.ReportSHA256 ||
		!stage.Report.ExpiresAt.Equal(stage.ExpiresAt) || s.store.read("transfer", id, subject, &transfer) != nil {
		migrationError(w, 409, "catchup_review_unavailable")
		return
	}
	var gate catchupBackupGate
	if s.BackupGate != nil {
		gate = func(ctx context.Context, report CatchupPreview) error {
			return s.BackupGate(ctx, MigrationPlan{TargetIdentitySHA256: report.TargetIdentitySHA256, TargetSchemaSHA256: report.TargetSchemaSHA256, TargetBaselineSHA256: report.TargetBaselineSHA256})
		}
	}
	receipt, err := s.applyCatchup(r.Context(), stage.Report, transfer.Manifest, subject, gate)
	if err != nil {
		migrationError(w, 409, "catchup_apply_conflicted_or_unavailable")
		return
	}
	migrationJSON(w, 200, receipt)
}

// applyCatchup binds backup, receipt ownership and post-commit baseline to the
// same reviewed preview. All mutations and the receipt share one transaction.
func (s *migrationServer) applyCatchup(ctx context.Context, reviewed CatchupPreview, manifest MigrationManifest, subject string, gate catchupBackupGate) (CatchupReceipt, error) {
	var empty CatchupReceipt
	if !migrationUUID(reviewed.RequestID) || !migrationSHA.MatchString(reviewed.ReportSHA256) || catchupPreviewHash(reviewed) != reviewed.ReportSHA256 || subject == "" {
		return empty, errors.New("invalid reviewed catchup report")
	}
	done, ok, err := readCatchupReceipt(ctx, s.DB, reviewed.RequestID, reviewed.ReportSHA256, subject)
	if err != nil {
		return empty, err
	}
	if ok {
		if err = s.captureCatchupCommittedBaseline(ctx, done, manifest, subject); err != nil {
			return empty, err
		}
		return done, nil
	}
	if gate == nil {
		return empty, errors.New("catchup backup proof required")
	}
	prepared, err := s.prepareCommittedCatchup(ctx, reviewed.BaselinePlanID, reviewed.BaselinePlanSHA256, subject, manifest)
	if err != nil {
		return empty, err
	}
	tx, err := migrationBegin(ctx, s.DB, false)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(674302318055)`); err != nil {
		return empty, errors.New("catchup transaction lock unavailable")
	}
	if err = migrationLock(ctx, tx, prepared.order); err != nil {
		return empty, err
	}
	done, ok, err = readCatchupReceiptTx(ctx, tx, reviewed.RequestID, reviewed.ReportSHA256, subject)
	if err != nil {
		return empty, err
	}
	if ok {
		if err = tx.Rollback(); err != nil {
			return empty, err
		}
		if err = s.captureCatchupCommittedBaseline(ctx, done, manifest, subject); err != nil {
			return empty, err
		}
		return done, nil
	}
	if err = executeCatchupRows(ctx, tx, prepared, manifest, reviewed, s.chunkReader(subject), gate); err != nil {
		return empty, err
	}
	if !time.Now().Before(reviewed.ExpiresAt) {
		return empty, errors.New("catchup report expired")
	}
	schema, err := migrationSchema(ctx, tx, prepared.order)
	if err != nil || schema != reviewed.TargetSchemaSHA256 {
		return empty, errors.New("catchup schema changed")
	}
	if err = gate(ctx, reviewed); err != nil {
		return empty, err
	}
	done = CatchupReceipt{Report: reviewed, AppliedAt: time.Now().UTC()}
	body, err := json.Marshal(done)
	if err != nil {
		return empty, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO accounts_migration_api.catchup_receipts(request_id,report_sha256,subject,receipt) VALUES($1,$2,$3,$4::jsonb)`, reviewed.RequestID, reviewed.ReportSHA256, subject, string(body)); err != nil {
		return empty, errors.New("catchup receipt insert failed")
	}
	if err = tx.Commit(); err != nil {
		return empty, errors.New("catchup commit uncertain; retry exact request")
	}
	if err = s.captureCatchupCommittedBaseline(ctx, done, manifest, subject); err != nil {
		return empty, errors.New("catchup committed but baseline unavailable; retry exact request")
	}
	return done, nil
}
