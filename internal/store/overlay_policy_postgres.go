package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

const overlayPolicyColumns = `network_id,revision,owner_user_uuid,name,source,artifact_canonical,artifact_sha256,compiler_version,warnings,status,generation,created_at,validated_at,activated_at`

func scanOverlayPolicy(row interface{ Scan(...any) error }) (*OverlayPolicyRevision, error) {
	var p OverlayPolicyRevision
	var activated sql.NullTime
	err := row.Scan(&p.NetworkID, &p.Revision, &p.OwnerUserID, &p.Name, &p.Source, &p.Artifact, &p.ArtifactSHA256, &p.CompilerVersion, &p.Warnings, &p.Status, &p.Generation, &p.CreatedAt, &p.ValidatedAt, &activated)
	if err != nil {
		return nil, err
	}
	p.Source = append([]byte(nil), p.Source...)
	p.Artifact = append([]byte(nil), p.Artifact...)
	p.Warnings = append([]byte(nil), p.Warnings...)
	p.CreatedAt = p.CreatedAt.UTC()
	p.ValidatedAt = p.ValidatedAt.UTC()
	if activated.Valid {
		v := activated.Time.UTC()
		p.ActivatedAt = &v
	}
	return &p, nil
}
func (s *postgresStore) CreateOverlayPolicyRevision(ctx context.Context, p *OverlayPolicyRevision, audit *AuditLog) error {
	if !validOverlayPolicy(p) {
		return errors.New("valid overlay policy revision is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-policy:' || $1,0))`, p.NetworkID); err != nil {
		return err
	}
	var next uint64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(revision),0)+1 FROM public.overlay_policy_revisions WHERE network_id=$1`, p.NetworkID).Scan(&next); err != nil {
		return err
	}
	if next != p.Revision {
		return ErrOverlayPolicyConflict
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO public.overlay_policy_revisions (network_id,revision,owner_user_uuid,name,source,artifact,artifact_canonical,artifact_sha256,compiler_version,warnings,status,generation) VALUES ($1,$2,$3,$4,$5::jsonb,$6::jsonb,$7,$8,$9,$10::jsonb,'draft',0) RETURNING created_at,validated_at`, p.NetworkID, p.Revision, p.OwnerUserID, p.Name, string(p.Source), string(p.Artifact), p.Artifact, p.ArtifactSHA256, p.CompilerVersion, string(p.Warnings)).Scan(&p.CreatedAt, &p.ValidatedAt)
	if err != nil {
		return err
	}
	p.Status = "draft"
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *postgresStore) GetOverlayPolicyRevision(ctx context.Context, networkID string, revision uint64) (*OverlayPolicyRevision, error) {
	p, err := scanOverlayPolicy(s.db.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2`, strings.TrimSpace(networkID), revision))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayPolicyNotFound
	}
	return p, err
}
func (s *postgresStore) GetLatestOverlayPolicyRevision(ctx context.Context, networkID string) (*OverlayPolicyRevision, error) {
	p, err := scanOverlayPolicy(s.db.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 ORDER BY revision DESC LIMIT 1`, strings.TrimSpace(networkID)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayPolicyNotFound
	}
	return p, err
}
func (s *postgresStore) ActivateOverlayPolicyRevision(ctx context.Context, networkID string, revision uint64, actor string, audit *AuditLog) (*OverlayPolicyRevision, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-policy:' || $1,0))`, networkID); err != nil {
		return nil, err
	}
	var owner, status string
	if err = tx.QueryRowContext(ctx, `SELECT owner_user_uuid::text,status FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2 FOR UPDATE`, networkID, revision).Scan(&owner, &status); errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayPolicyNotFound
	} else if err != nil {
		return nil, err
	}
	_, _ = owner, actor
	if status != "active" {
		var generation uint64
		if err = tx.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(MAX(generation),0),1)+1 FROM public.overlay_policy_revisions WHERE network_id=$1`, networkID).Scan(&generation); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_policy_revisions SET status='superseded' WHERE network_id=$1 AND status='active'`, networkID); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_policy_revisions SET status='active',generation=$3,activated_at=now() WHERE network_id=$1 AND revision=$2`, networkID, revision, generation); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_policy_builds(network_id,generation,revision,artifact,artifact_canonical,artifact_sha256,compiler_version) SELECT network_id,generation,revision,artifact,artifact_canonical,artifact_sha256,compiler_version FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2 AND artifact_canonical IS NOT NULL`, networkID, revision); err != nil {
			return nil, err
		}
		var canonicalReady bool
		if err = tx.QueryRowContext(ctx, `SELECT artifact_canonical IS NOT NULL FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2`, networkID, revision).Scan(&canonicalReady); err != nil || !canonicalReady {
			return nil, errors.New("policy canonical artifact must be recompiled before activation")
		}
		if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
			return nil, err
		}
	}
	p, err := scanOverlayPolicy(tx.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2`, networkID, revision))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return p, nil
}
func (s *postgresStore) GetActiveOverlayPolicy(ctx context.Context, networkID string) (*OverlayPolicyRevision, error) {
	p, err := scanOverlayPolicy(s.db.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 AND status='active'`, strings.TrimSpace(networkID)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOverlayPolicyNotFound
	}
	return p, err
}
func (s *postgresStore) RefreshOverlayPolicyBuild(ctx context.Context, networkID string, revision uint64, expected string, artifact []byte, digest, compiler string, audit *AuditLog) (*OverlayPolicyRevision, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('overlay-policy:' || $1,0))`, networkID); err != nil {
		return nil, false, err
	}
	p, err := scanOverlayPolicy(tx.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2 AND status='active' FOR UPDATE`, networkID, revision))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, ErrOverlayPolicyConflict
	} else if err != nil {
		return nil, false, err
	}
	if p.ArtifactSHA256 == digest && len(p.Artifact) > 0 {
		return p, false, tx.Commit()
	}
	if p.ArtifactSHA256 != expected {
		return nil, false, ErrOverlayPolicyConflict
	}
	var generation uint64
	if err = tx.QueryRowContext(ctx, `SELECT GREATEST(COALESCE((SELECT MAX(generation) FROM public.overlay_policy_revisions WHERE network_id=$1),0),COALESCE((SELECT MAX(generation) FROM public.overlay_policy_builds WHERE network_id=$1),0),1)+1`, networkID).Scan(&generation); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_policy_builds(network_id,generation,revision,artifact,artifact_canonical,artifact_sha256,compiler_version) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7)`, networkID, generation, revision, string(artifact), artifact, digest, compiler); err != nil {
		return nil, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE public.overlay_policy_revisions SET artifact=$3::jsonb,artifact_canonical=$4,artifact_sha256=$5,compiler_version=$6,generation=$7,validated_at=now() WHERE network_id=$1 AND revision=$2`, networkID, revision, string(artifact), artifact, digest, compiler, generation); err != nil {
		return nil, false, err
	}
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return nil, false, err
	}
	p, err = scanOverlayPolicy(tx.QueryRowContext(ctx, `SELECT `+overlayPolicyColumns+` FROM public.overlay_policy_revisions WHERE network_id=$1 AND revision=$2`, networkID, revision))
	if err != nil {
		return nil, false, err
	}
	if err = tx.Commit(); err != nil {
		return nil, false, err
	}
	return p, true, nil
}
func (s *postgresStore) UpdateOverlayDeviceTags(ctx context.Context, userID, networkID, deviceID string, tags []string, audit *AuditLog) error {
	raw, _ := json.Marshal(tags)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var targetOwner string
	if err = tx.QueryRowContext(ctx, `SELECT user_uuid::text FROM public.overlay_devices WHERE network_id=$1 AND id=$2 FOR UPDATE`, networkID, deviceID).Scan(&targetOwner); err != nil {
		return err
	}
	_ = userID
	_, err = tx.ExecContext(ctx, `INSERT INTO public.overlay_device_projection_metadata(user_uuid,device_id,tags,attachments,source_kind,source_variable,baseline_sha256) VALUES($1,$2,$3::jsonb,'[]'::jsonb,'dynamic-policy','api','0000000000000000000000000000000000000000000000000000000000000000') ON CONFLICT(user_uuid,device_id) DO UPDATE SET tags=EXCLUDED.tags,updated_at=now()`, targetOwner, deviceID, string(raw))
	if err != nil {
		return err
	}
	if err = insertOverlayJoinAuditTx(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}
