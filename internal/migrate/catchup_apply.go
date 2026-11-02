package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

type catchupReceipt struct {
	Report    CatchupPreview `json:"report"`
	AppliedAt time.Time      `json:"applied_at"`
}

func readCatchupReceiptTx(ctx context.Context, tx *sql.Tx, id, hash, subject string) (catchupReceipt, bool, error) {
	var result catchupReceipt
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
		return catchupReceipt{}, false, errors.New("catchup receipt binding mismatch")
	}
	identity, e := migrationIdentity(ctx, tx)
	if e != nil || identity != result.Report.TargetIdentitySHA256 {
		return catchupReceipt{}, false, errors.New("catchup receipt target mismatch")
	}
	return result, true, nil
}

func readCatchupReceipt(ctx context.Context, db *sql.DB, id, hash, subject string) (catchupReceipt, bool, error) {
	tx, err := migrationBegin(ctx, db, true)
	if err != nil {
		return catchupReceipt{}, false, err
	}
	defer tx.Rollback()
	return readCatchupReceiptTx(ctx, tx, id, hash, subject)
}

// applyCatchup is internal until backup, receipt ownership, API authorization,
// next-baseline recovery and real-scale behavior are validated end to end.
func (s *migrationServer) applyCatchup(ctx context.Context, reviewed CatchupPreview, manifest MigrationManifest, subject string, gate catchupBackupGate) (catchupReceipt, error) {
	var empty catchupReceipt
	if !migrationUUID(reviewed.RequestID) || !migrationSHA.MatchString(reviewed.ReportSHA256) || catchupPreviewHash(reviewed) != reviewed.ReportSHA256 || subject == "" {
		return empty, errors.New("invalid reviewed catchup report")
	}
	done, ok, err := readCatchupReceipt(ctx, s.DB, reviewed.RequestID, reviewed.ReportSHA256, subject)
	if err != nil {
		return empty, err
	}
	if ok {
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
	done = catchupReceipt{Report: reviewed, AppliedAt: time.Now().UTC()}
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
	return done, nil
}
