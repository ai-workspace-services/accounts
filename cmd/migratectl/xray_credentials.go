package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/spf13/cobra"
)

type xrayClientConfig struct {
	Inbounds []struct {
		Settings struct {
			Clients []struct {
				ID string `json:"id"`
			} `json:"clients"`
		} `json:"settings"`
	} `json:"inbounds"`
}

// newImportXrayCredentialsCmd imports existing Xray client IDs as bridge
// credentials. It deliberately does not connect to or alter Xray.
func newImportXrayCredentialsCmd() *cobra.Command {
	var dsn, tenantID, configPath string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "import-xray-credentials",
		Short: "Import unchanged Xray client ids as initial bridge credentials",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(dsn) == "" || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(configPath) == "" {
				return errors.New("--dsn, --tenant-id and --xray-config are required")
			}
			payload, err := os.ReadFile(configPath)
			if err != nil {
				return fmt.Errorf("read xray config: %w", err)
			}
			var config xrayClientConfig
			if err := json.Unmarshal(payload, &config); err != nil {
				return fmt.Errorf("parse xray config: %w", err)
			}
			ids, err := xrayClientIDs(config)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			return importXrayCredentials(ctx, cmd.OutOrStdout(), dsn, tenantID, ids, dryRun)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	cmd.Flags().StringVar(&tenantID, "tenant-id", "", "Tenant that owns the imported credentials")
	cmd.Flags().StringVar(&configPath, "xray-config", "", "Read-only copy of Xray config.json")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate mappings without writing bridge_credentials")
	return cmd
}

func xrayClientIDs(config xrayClientConfig) ([]string, error) {
	seen := make(map[string]struct{})
	ids := make([]string, 0)
	for _, inbound := range config.Inbounds {
		for _, client := range inbound.Settings.Clients {
			id := strings.TrimSpace(client.ID)
			if id == "" {
				continue
			}
			if _, err := uuid.Parse(id); err != nil {
				return nil, fmt.Errorf("invalid Xray client id %q: %w", id, err)
			}
			if _, exists := seen[id]; !exists {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("no Xray client ids found")
	}
	return ids, nil
}

func importXrayCredentials(ctx context.Context, output interface{ Write([]byte) (int, error) }, dsn, tenantID string, ids []string, dryRun bool) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		return err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var tenantExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.tenants WHERE id = $1)`, strings.TrimSpace(tenantID)).Scan(&tenantExists); err != nil {
		return fmt.Errorf("verify tenant: %w", err)
	}
	if !tenantExists {
		return fmt.Errorf("tenant %q does not exist", tenantID)
	}

	imported := 0
	for _, credentialID := range ids {
		var userID string
		if err := tx.QueryRowContext(ctx, `SELECT uuid::text FROM public.users WHERE proxy_uuid = $1::uuid`, credentialID).Scan(&userID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("xray credential %s does not match users.proxy_uuid", credentialID)
			}
			return err
		}
		var existingUserID string
		err := tx.QueryRowContext(ctx, `SELECT user_uuid::text FROM public.bridge_credentials WHERE credential_uuid = $1::uuid`, credentialID).Scan(&existingUserID)
		switch {
		case err == nil && existingUserID != userID:
			return fmt.Errorf("credential %s is already assigned to another user", credentialID)
		case err == nil:
			continue
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		if !dryRun {
			if _, err := tx.ExecContext(ctx, `INSERT INTO public.bridge_credentials (credential_uuid, user_uuid, tenant_id, source) VALUES ($1::uuid, $2::uuid, $3, 'xray-import')`, credentialID, userID, tenantID); err != nil {
				return fmt.Errorf("insert credential %s: %w", credentialID, err)
			}
		}
		imported++
	}
	if dryRun {
		if err := tx.Rollback(); err != nil {
			return err
		}
	} else if err := tx.Commit(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "validated=%d inserted=%d dry_run=%t; users table was not modified\n", len(ids), imported, dryRun)
	return err
}
