package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"account/internal/migrate"
	"github.com/spf13/cobra"
)

func bindGenerationUserOptions(cmd *cobra.Command, options *migrate.GenerationUserOptions, sourceEnv, targetEnv *string) {
	cmd.Flags().StringVar(sourceEnv, "source-dsn-env", "", "Environment variable containing the read-only source DSN")
	cmd.Flags().StringVar(targetEnv, "target-dsn-env", "", "Environment variable containing the target DSN")
	cmd.Flags().StringVar(&options.SourceGeneration, "source-generation", "", "Source generation, for example prod-v3")
	cmd.Flags().StringVar(&options.TargetGeneration, "target-generation", "", "Immediately following target generation, for example prod-v4")
	cmd.Flags().StringVar(&options.Environment, "environment", "prod", "Explicit target environment")
	cmd.Flags().StringVar(&options.SchemaSHA256, "schema-sha256", "", "Exact reviewed Accounts native schema SHA-256")
	cmd.Flags().StringVar(&options.BillingSHA256, "billing-schema-sha256", "", "Exact reviewed Billing schema SHA-256")
	cmd.Flags().UintVar(&options.ExpectedTargetVersion, "expected-target-version", 0, "Exact clean target migration version")
}

func generationUserDSNs(sourceEnv, targetEnv string) (string, string, error) {
	if strings.TrimSpace(sourceEnv) == "" || strings.TrimSpace(targetEnv) == "" || sourceEnv == targetEnv {
		return "", "", errors.New("distinct --source-dsn-env and --target-dsn-env are required")
	}
	source, target := strings.TrimSpace(os.Getenv(sourceEnv)), strings.TrimSpace(os.Getenv(targetEnv))
	if source == "" || target == "" {
		return "", "", errors.New("source or target DSN environment variable is empty")
	}
	return source, target, nil
}

func newCaptureGenerationUserBaselineCmd() *cobra.Command {
	var sourceEnv, targetEnv string
	options := migrate.GenerationUserOptions{}
	cmd := &cobra.Command{Use: "capture-user-baseline", Args: cobra.NoArgs,
		Short: "Capture a privacy-preserving source user-ID baseline before DNS handoff",
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, target, err := generationUserDSNs(sourceEnv, targetEnv)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			baseline, err := migrate.CaptureGenerationUserBaseline(ctx, source, target, options)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(baseline)
		}}
	bindGenerationUserOptions(cmd, &options, &sourceEnv, &targetEnv)
	return cmd
}

func newCheckLateGenerationUsersCmd() *cobra.Command {
	var sourceEnv, targetEnv, baselineFile string
	options := migrate.GenerationUserOptions{}
	cmd := &cobra.Command{Use: "check-late-users", Args: cobra.NoArgs,
		Short: "Find source user IDs created after the cutover baseline and compare identities",
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, target, err := generationUserDSNs(sourceEnv, targetEnv)
			if err != nil {
				return err
			}
			if strings.TrimSpace(baselineFile) == "" {
				return errors.New("--baseline-file is required")
			}
			data, err := os.ReadFile(baselineFile)
			if err != nil {
				return errors.New("cannot read protected generation user baseline")
			}
			var baseline migrate.GenerationUserBaseline
			if json.Unmarshal(data, &baseline) != nil {
				return errors.New("generation user baseline is invalid JSON")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			audit, err := migrate.DetectLateGenerationUsers(ctx, source, target, options, baseline)
			if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(audit); outputErr != nil {
				return outputErr
			}
			if err != nil {
				return err
			}
			if audit.SourceWriteIsolationAnomaly {
				return errors.New("source write-isolation anomaly or identity mismatch detected; see aggregate audit output")
			}
			return nil
		}}
	bindGenerationUserOptions(cmd, &options, &sourceEnv, &targetEnv)
	cmd.Flags().StringVar(&baselineFile, "baseline-file", "", "Protected pre-cutover capture-user-baseline JSON file")
	return cmd
}

func newSyncSingleGenerationUserCmd() *cobra.Command {
	var sourceEnv, targetEnv, sourceUserIDEnv, confirmUserIDEnv string
	options := migrate.SingleGenerationUserOptions{}
	cmd := &cobra.Command{Use: "sync-user", Args: cobra.NoArgs,
		Short: "Preview or transactionally add one missing source account and its OAuth identities",
		RunE: func(cmd *cobra.Command, _ []string) error {
			source, target, err := generationUserDSNs(sourceEnv, targetEnv)
			if err != nil {
				return err
			}
			if strings.TrimSpace(sourceUserIDEnv) == "" {
				return errors.New("--source-user-id-env is required")
			}
			options.SourceUserID = strings.TrimSpace(os.Getenv(sourceUserIDEnv))
			if confirmUserIDEnv != "" {
				options.ConfirmSourceUserID = strings.TrimSpace(os.Getenv(confirmUserIDEnv))
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
			defer cancel()
			receipt, err := migrate.SyncSingleGenerationUser(ctx, source, target, options)
			if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); outputErr != nil {
				return outputErr
			}
			return err
		}}
	bindGenerationUserOptions(cmd, &options.GenerationUserOptions, &sourceEnv, &targetEnv)
	cmd.Flags().StringVar(&sourceUserIDEnv, "source-user-id-env", "", "Environment variable containing the selected stable source user UUID")
	cmd.Flags().StringVar(&confirmUserIDEnv, "confirm-source-user-id-env", "", "Environment variable repeating the UUID for explicit apply confirmation")
	cmd.Flags().BoolVar(&options.Apply, "apply", false, "Insert only if preflight proves the source user is absent from the target; default is preview")
	cmd.Flags().BoolVar(&options.SourceWritersPaused, "source-writers-paused", false, "Execution owner verified old-source application writers are isolated")
	cmd.Flags().StringVar(&options.ExpectedPlanSHA256, "expected-plan-sha256", "", "Exact plan checksum from the unchanged preview")
	return cmd
}
