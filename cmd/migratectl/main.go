package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"account/internal/migrate"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	defaultMigrationDir = "sql/migrations"
	defaultSchemaFile   = "sql/schema.sql"
)

func main() {
	ctx := context.Background()
	rootCmd := newRootCmd()
	if err := rootCmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var migrationDir string
	cmd := &cobra.Command{
		Use:   "migratectl",
		Short: "XWorkmate account database migration orchestrator",
	}

	migrationDir = defaultMigrationDir
	cmd.PersistentFlags().StringVar(&migrationDir, "dir", migrationDir, "directory containing migration files")

	cmd.AddCommand(newMigrateCmd(&migrationDir))
	cmd.AddCommand(newCleanCmd())
	cmd.AddCommand(newCheckCmd())
	cmd.AddCommand(newVerifyCmd())
	cmd.AddCommand(newResetCmd(&migrationDir))
	cmd.AddCommand(newVersionCmd(&migrationDir))
	cmd.AddCommand(newExportCmd())
	cmd.AddCommand(newImportCmd())
	cmd.AddCommand(newImportXrayCredentialsCmd())

	return cmd
}

func newMigrateCmd(dir *string) *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dsn == "" {
				return errors.New("--dsn is required")
			}
			runner := migrate.NewRunner(*dir)
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			return runner.Up(ctx, dsn)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	return cmd
}

func newCleanCmd() *cobra.Command {
	var (
		dsn   string
		force bool
	)
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove invalid indexes while preserving tables and triggers",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dsn == "" {
				return errors.New("--dsn is required")
			}
			cleaner := migrate.NewCleaner()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			return cleaner.Clean(ctx, dsn, force)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	cmd.Flags().BoolVar(&force, "force", false, "Confirm clean-up actions")
	return cmd
}

func newCheckCmd() *cobra.Command {
	var (
		cnDSN     string
		globalDSN string
		autoFix   bool
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Compare CN and Global schemas",
		RunE: func(cmd *cobra.Command, args []string) error {
			checker := migrate.NewChecker()
			ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
			defer cancel()
			return checker.Check(ctx, cnDSN, globalDSN, autoFix)
		},
	}
	cmd.Flags().StringVar(&cnDSN, "cn", "", "CN region PostgreSQL DSN")
	cmd.Flags().StringVar(&globalDSN, "global", "", "Global region PostgreSQL DSN")
	cmd.Flags().BoolVar(&autoFix, "auto-fix", false, "Automatically apply missing statements to CN")
	return cmd
}

func newVerifyCmd() *cobra.Command {
	var (
		dsn        string
		schemaPath string
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify that the database matches schema.sql",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dsn == "" {
				return errors.New("--dsn is required")
			}
			if schemaPath == "" {
				schemaPath = defaultSchemaFile
			}
			verifier := migrate.NewVerifier()
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			return verifier.Verify(ctx, dsn, schemaPath)
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	cmd.Flags().StringVar(&schemaPath, "schema", defaultSchemaFile, "Path to schema.sql reference file")
	return cmd
}

func newResetCmd(_ *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Disabled: destructive database reset is prohibited",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("database reset is disabled; apply forward-only migrations with migratectl migrate")
		},
	}
	return cmd
}

func newVersionCmd(dir *string) *cobra.Command {
	var dsn string
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Show current migration version",
		RunE: func(cmd *cobra.Command, args []string) error {
			if dsn == "" {
				return errors.New("--dsn is required")
			}
			runner := migrate.NewRunner(*dir)
			version, dirty, err := runner.Version(dsn)
			if err != nil {
				return err
			}
			if dirty {
				fmt.Printf("Current migration version: %d (dirty)\n", version)
			} else {
				fmt.Printf("Current migration version: %d\n", version)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	return cmd
}

func newExportCmd() *cobra.Command {
	var (
		dsn          string
		dsnEnv       string
		accountsOnly bool
		email        string
		output       string
		timeout      time.Duration
	)

	output = "account-export.yaml"
	timeout = 2 * time.Minute

	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export user data to a YAML snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			dsn, err = resolveTransferDSN(dsn, dsnEnv)
			if err != nil {
				return err
			}
			if dsn == "" {
				return errors.New("--dsn is required")
			}

			exporter := migrate.NewExporter()
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			var dump *migrate.AccountDump
			if accountsOnly {
				if email != "" || output == "-" {
					return errors.New("accounts-only forbids partial email exports and stdout snapshots")
				}
				dump, err = exporter.ExportAccountsOnly(ctx, dsn)
			} else {
				dump, err = exporter.Export(ctx, dsn, email)
			}
			if err != nil {
				return err
			}

			var buf bytes.Buffer
			encoder := yaml.NewEncoder(&buf)
			encoder.SetIndent(2)
			if err := encoder.Encode(dump); err != nil {
				encoder.Close()
				return fmt.Errorf("encode yaml: %w", err)
			}
			if err := encoder.Close(); err != nil {
				return fmt.Errorf("finalize yaml: %w", err)
			}

			switch output {
			case "-":
				_, err = cmd.OutOrStdout().Write(buf.Bytes())
				return err
			case "":
				return errors.New("--output must not be empty")
			default:
				if err := os.WriteFile(output, buf.Bytes(), 0o600); err != nil {
					return err
				}
				count := len(dump.Users)
				if dump.ThreeTables != nil {
					count = len(dump.ThreeTables.Rows["users"])
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Exported %d users to protected snapshot file\n", count)
				return nil
			}
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	cmd.Flags().StringVar(&email, "email", "", "Case-insensitive email keyword filter")
	cmd.Flags().StringVar(&dsnEnv, "dsn-env", "", "Environment variable containing transfer DSN (not supported by migrate)")
	cmd.Flags().BoolVar(&accountsOnly, "accounts-only", false, "Export all three account tables without dropping source fields")
	cmd.Flags().StringVar(&output, "output", output, "Output file path or '-' for stdout")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Export operation timeout")

	return cmd
}

func newImportCmd() *cobra.Command {
	var (
		targetDatabase        string
		dsn                   string
		dsnEnv                string
		accountsOnly          bool
		file                  string
		timeout               time.Duration
		merge                 bool
		mergeStrategy         string
		dryRun                bool
		regenerateUserUUIDs   bool
		preserveExistingUsers bool
		skipSessions          bool
		mergeAllowlist        []string
	)

	timeout = 5 * time.Minute

	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import user data from a YAML snapshot",
		RunE: func(cmd *cobra.Command, args []string) error {
			var resolveErr error
			dsn, resolveErr = resolveTransferDSN(dsn, dsnEnv)
			if resolveErr != nil {
				return resolveErr
			}
			if dsn == "" {
				return errors.New("--dsn is required")
			}
			if file == "" {
				return errors.New("--file is required")
			}

			var (
				data []byte
				err  error
			)

			if file == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}

			var dump migrate.AccountDump
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			if err := decoder.Decode(&dump); err != nil {
				return errors.New("invalid snapshot YAML or unknown field (details suppressed)")
			}
			var extra any
			if err := decoder.Decode(&extra); err != io.EOF {
				return errors.New("snapshot must contain exactly one YAML document")
			}

			importer := migrate.NewImporter()
			allowlist := map[string]struct{}{}
			for _, id := range mergeAllowlist {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				allowlist[id] = struct{}{}
			}
			if len(allowlist) == 0 {
				allowlist = nil
			}
			if !merge {
				if mergeStrategy != "" {
					return errors.New("--merge-strategy requires --merge")
				}
				if len(mergeAllowlist) > 0 {
					return errors.New("--merge-allowlist requires --merge")
				}
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()

			report, err := importer.Import(ctx, dsn, &dump, migrate.ImportOptions{
				TargetDatabase:        targetDatabase,
				AccountsOnly:          accountsOnly,
				Merge:                 merge,
				MergeStrategy:         migrate.MergeStrategy(mergeStrategy),
				DryRun:                dryRun,
				PreserveExistingUsers: preserveExistingUsers,
				SkipSessions:          skipSessions,
				RegenerateUserUUIDs:   regenerateUserUUIDs,
				Allowlist:             allowlist,
				LogWriter:             cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			summaryTarget := "applied"
			if dryRun {
				summaryTarget = "preview"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Import %s: users inserted=%d updated=%d skipped=%d\n", summaryTarget, report.UsersInserted, report.UsersUpdated, report.UsersSkipped)
			fmt.Fprintf(cmd.OutOrStdout(), "Identities inserted=%d updated=%d deleted=%d\n", report.IdentitiesInserted, report.IdentitiesUpdated, report.IdentitiesDeleted)
			fmt.Fprintf(cmd.OutOrStdout(), "Sessions inserted=%d updated=%d deleted=%d\n", report.SessionsInserted, report.SessionsUpdated, report.SessionsDeleted)
			if report.ConflictsResolved > 0 || report.ConflictsSkipped > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "Conflicts resolved=%d skipped=%d\n", report.ConflictsResolved, report.ConflictsSkipped)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&dsn, "dsn", "", "PostgreSQL connection string")
	cmd.Flags().StringVar(&file, "file", "", "YAML file path or '-' for stdin")
	cmd.Flags().StringVar(&targetDatabase, "target-database", "", "Explicit independent accounts_rebuild_<id> database identity for accounts-only")
	cmd.Flags().StringVar(&dsnEnv, "dsn-env", "", "Environment variable containing transfer DSN (not supported by migrate)")
	cmd.Flags().BoolVar(&accountsOnly, "accounts-only", false, "Lossless three-table import into an independent target; exact replay only")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "Import operation timeout")
	cmd.Flags().BoolVar(&merge, "merge", false, "Enable additive merge behaviour")
	cmd.Flags().StringVar(&mergeStrategy, "merge-strategy", "", "Merge strategy (replace, append, timestamp)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview the import without applying changes")
	cmd.Flags().BoolVar(&preserveExistingUsers, "preserve-existing-users", false, "Deprecated: merge mode always preserves existing user profiles")
	cmd.Flags().BoolVar(&skipSessions, "skip-sessions", false, "Do not import source login sessions")
	cmd.Flags().BoolVar(&regenerateUserUUIDs, "regenerate-user-uuids", false, "Assign new target identity UUIDs while preserving proxy UUIDs")
	cmd.Flags().StringSliceVar(&mergeAllowlist, "merge-allowlist", nil, "User UUIDs allowed to merge (comma-separated or repeated)")

	return cmd
}

func resolveTransferDSN(dsn, name string) (string, error) {
	if name == "" {
		return dsn, nil
	}
	if dsn != "" {
		return "", errors.New("choose --dsn or --dsn-env, not both")
	}
	for i, r := range name {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return "", errors.New("invalid DSN environment variable name")
		}
	}
	value := os.Getenv(name)
	if value == "" {
		return "", errors.New("DSN environment variable is empty")
	}
	return value, nil
}
