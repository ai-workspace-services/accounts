package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"account/internal/migrate"
	"github.com/spf13/cobra"
)

func newUsageMergeCmd() *cobra.Command { return newReconcileCmd(false) }

func newBusinessAppendCmd() *cobra.Command { return newReconcileCmd(true) }

func newReconcileCmd(appendBusiness bool) *cobra.Command {
	var sourceEnv, targetEnv string
	options := migrate.UsageMergeOptions{AppendBusiness: appendBusiness}
	name := "reconcile-usage"
	if appendBusiness {
		name = "reconcile-business"
	}
	cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, Short: "Preview or transactionally reconcile reviewed business facts between adjacent generations without schema changes", RunE: func(cmd *cobra.Command, args []string) error {
		if sourceEnv == "" || targetEnv == "" || sourceEnv == targetEnv {
			return errors.New("distinct source and target DSN environment names are required")
		}
		source, target := os.Getenv(sourceEnv), os.Getenv(targetEnv)
		if source == "" || target == "" {
			return errors.New("source or target DSN environment is empty")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		receipt, err := migrate.ReconcileUsage(ctx, source, target, options)
		// Preserve safe conflict previews even when the process exits nonzero.
		if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); outputErr != nil {
			return outputErr
		}
		return err
	}}
	cmd.Flags().StringVar(&sourceEnv, "source-dsn-env", "", "Environment variable containing the read-only source database DSN")
	cmd.Flags().StringVar(&targetEnv, "target-dsn-env", "", "Environment variable containing the target database DSN")
	cmd.Flags().StringVar(&options.SourceGeneration, "source-generation", "", "Source generation (for example prod-v3)")
	cmd.Flags().StringVar(&options.TargetGeneration, "target-generation", "", "Immediately following target generation (for example prod-v4)")
	cmd.Flags().StringVar(&options.SchemaSHA256, "schema-sha256", "", "Exact compiled native schema SHA-256")
	cmd.Flags().StringVar(&options.BillingSHA256, "billing-schema-sha256", "", "Exact compiled Billing schema SHA-256")
	cmd.Flags().UintVar(&options.ExpectedTargetVersion, "expected-target-version", 0, "Exact clean target migration checkpoint")
	cmd.Flags().BoolVar(&options.Apply, "apply", false, "Commit the reviewed conflict-free plan; default is read-only preview")
	if appendBusiness {
		cmd.Flags().BoolVar(&options.IgnoreDefaultSandboxProxyDrift, "ignore-default-sandbox-proxy-drift", false, "Preserve the default Sandbox proxy UUID when email and password hash match; other identity drift remains blocked")
	}
	if !appendBusiness {
		cmd.Flags().BoolVar(&options.WritersPaused, "writers-paused", false, "Execution owner has quiesced target writers")
	}
	cmd.Flags().StringVar(&options.ExpectedPlanSHA256, "expected-plan-sha256", "", "SHA-256 from the unchanged read-only preview")
	return cmd
}
