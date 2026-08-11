package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// syncRotatedCredentialUUIDs keeps the legacy Xray field and the new
// tenant-scoped credential rows aligned during an explicit UUID rotation.
// It never changes token_hash, so UUID and token rotation remain independent.
func (h *handler) syncRotatedCredentialUUIDs(ctx context.Context, userID, preferredUUID string) error {
	if h == nil || h.db == nil {
		return nil
	}
	type credentialRow struct {
		CredentialUUID string `gorm:"column:credential_uuid"`
	}
	var rows []credentialRow
	tx := h.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		return tx.Error
	}
	if err := tx.Raw(`SELECT credential_uuid::text FROM public.bridge_credentials WHERE user_uuid = ?::uuid AND status = 'active' ORDER BY tenant_id`, userID).Scan(&rows).Error; err != nil {
		tx.Rollback()
		return err
	}
	for index, row := range rows {
		newID := strings.TrimSpace(preferredUUID)
		if index > 0 || newID == "" {
			id, err := uuid.NewV7()
			if err != nil {
				tx.Rollback()
				return fmt.Errorf("generate rotated credential uuid: %w", err)
			}
			newID = id.String()
		}
		if err := tx.Exec(`UPDATE public.bridge_credentials SET credential_uuid = ?::uuid, updated_at = now() WHERE credential_uuid = ?::uuid`, newID, row.CredentialUUID).Error; err != nil {
			tx.Rollback()
			return fmt.Errorf("update bridge credential uuid: %w", err)
		}
	}
	if err := tx.Commit().Error; err != nil {
		return err
	}
	return nil
}
