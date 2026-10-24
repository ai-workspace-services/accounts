package main

import (
	"account/internal/migrate"
	"context"
	"encoding/json"
	"errors"
	"github.com/spf13/cobra"
	"os"
	"time"
)

func newIdentityReconcileCmd() *cobra.Command {
	var sourceEnv, targetEnv string
	var opt migrate.IdentityReconcileOptions
	cmd := &cobra.Command{Use: "reconcile-identities", Short: "Preview or repair existing identity profiles using an exact locked plan", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		if sourceEnv == "" || targetEnv == "" || sourceEnv == targetEnv || os.Getenv(sourceEnv) == "" || os.Getenv(targetEnv) == "" {
			return errors.New("distinct nonempty source/target DSN environment variables required")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Minute)
		defer cancel()
		receipt, err := migrate.ReconcileIdentities(ctx, os.Getenv(sourceEnv), os.Getenv(targetEnv), opt)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	}}
	f := cmd.Flags()
	f.StringVar(&sourceEnv, "source-dsn-env", "", "Readonly source DSN variable")
	f.StringVar(&targetEnv, "target-dsn-env", "", "Target DSN variable")
	f.StringVar(&opt.Environment, "environment", "", "Explicit prod or uat")
	f.StringVar(&opt.SchemaSHA256, "schema-sha256", "", "Exact native schema checksum")
	f.BoolVar(&opt.Apply, "apply", false, "Apply exact reviewed plan; default read-only preview")
	f.StringVar(&opt.ExpectedPlanSHA256, "expected-plan-sha256", "", "Exact unchanged preview SHA256")
	return cmd
}
