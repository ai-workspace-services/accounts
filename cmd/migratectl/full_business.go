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

func newFullBusinessCmd(compare bool) *cobra.Command {
	var sourceEnv, targetEnv string
	options := migrate.FullBusinessOptions{CompareOnly: compare}
	name := "copy-full-business"
	if compare {
		name = "compare-full-business"
	}
	cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, Short: "Stream and verify the reviewed 53-table business scope without source writes", RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(sourceEnv) == "" || strings.TrimSpace(targetEnv) == "" || sourceEnv == targetEnv {
			return errors.New("distinct --source-dsn-env and --target-dsn-env are required")
		}
		source := strings.TrimSpace(os.Getenv(sourceEnv))
		target := strings.TrimSpace(os.Getenv(targetEnv))
		if source == "" || target == "" {
			return errors.New("source or target DSN environment variable is empty")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		receipt, err := migrate.CopyFullBusiness(ctx, source, target, options)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	}}
	cmd.Flags().StringVar(&sourceEnv, "source-dsn-env", "", "Environment variable containing the dedicated readonly_release source DSN")
	cmd.Flags().StringVar(&targetEnv, "target-dsn-env", "", "Environment variable containing the native target DSN")
	cmd.Flags().StringVar(&options.Environment, "environment", "", "Explicit uat or prod target")
	cmd.Flags().StringVar(&options.SchemaSHA256, "schema-sha256", "", "Exact compiled Accounts native schema SHA-256")
	cmd.Flags().StringVar(&options.BillingSHA256, "billing-schema-sha256", "", "Exact reviewed additive Billing schema SHA-256")
	cmd.Flags().BoolVar(&options.WritersPaused, "writers-paused", false, "Execution owner verified paused target writers")
	cmd.Flags().BoolVar(&options.DryRun, "dry-run", true, "Inspect catalogs and user matching without reading large tables or writing rows")
	return cmd
}

func newCoreUsersCmd(compare bool) *cobra.Command {
	var sourceEnv, targetEnv string
	options := migrate.CoreUsersOptions{CompareOnly: compare}
	name := "copy-core-users"
	if compare {
		name = "compare-core-users"
	}
	cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, Short: "Copy or compare the email, password hash and Proxy UUID user contract", RunE: func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(sourceEnv) == "" || strings.TrimSpace(targetEnv) == "" || sourceEnv == targetEnv {
			return errors.New("distinct --source-dsn-env and --target-dsn-env are required")
		}
		source := strings.TrimSpace(os.Getenv(sourceEnv))
		target := strings.TrimSpace(os.Getenv(targetEnv))
		if source == "" || target == "" {
			return errors.New("source or target DSN environment variable is empty")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
		defer cancel()
		receipt, err := migrate.CopyCoreUsers(ctx, source, target, options)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	}}
	cmd.Flags().StringVar(&sourceEnv, "source-dsn-env", "", "Environment variable containing the dedicated readonly_release source DSN")
	cmd.Flags().StringVar(&targetEnv, "target-dsn-env", "", "Environment variable containing the native target DSN")
	cmd.Flags().StringVar(&options.Environment, "environment", "", "Explicit uat or prod target")
	cmd.Flags().StringVar(&options.SchemaSHA256, "schema-sha256", "", "Exact compiled Accounts native schema SHA-256")
	cmd.Flags().StringVar(&options.BillingSHA256, "billing-schema-sha256", "", "Exact reviewed additive Billing schema SHA-256")
	cmd.Flags().BoolVar(&options.WritersPaused, "writers-paused", false, "Execution owner verified paused target writers")
	return cmd
}
