package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"account/internal/migrate"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
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
	var sourceEnv, targetEnv, outputFormat string
	options := migrate.CoreUsersOptions{CompareOnly: compare}
	name := "copy-core-users"
	if compare {
		name = "compare-core-users"
	}
	cmd := &cobra.Command{Use: name, Args: cobra.NoArgs, Short: "Copy or compare the email, password hash and Proxy UUID user contract", RunE: func(cmd *cobra.Command, args []string) error {
		return runCoreUsersCompare(cmd, sourceEnv, targetEnv, options, compare, outputFormat)
	}}
	cmd.Flags().StringVar(&sourceEnv, "source-dsn-env", "", "Environment variable containing the dedicated readonly_release source DSN")
	cmd.Flags().StringVar(&targetEnv, "target-dsn-env", "", "Environment variable containing the native target DSN")
	cmd.Flags().StringVar(&options.Environment, "environment", "", "Explicit uat or prod target")
	cmd.Flags().StringVar(&options.SchemaSHA256, "schema-sha256", "", "Exact compiled Accounts native schema SHA-256")
	cmd.Flags().StringVar(&options.BillingSHA256, "billing-schema-sha256", "", "Exact reviewed additive Billing schema SHA-256")
	cmd.Flags().BoolVar(&options.WritersPaused, "writers-paused", false, "Execution owner verified paused target writers")
	if compare {
		cmd.Flags().StringVarP(&outputFormat, "output", "o", "raw", "safe diff output: raw (legacy JSON), yaml, or markdown")
	}
	return cmd
}

func runCoreUsersCompare(cmd *cobra.Command, sourceEnv, targetEnv string, options migrate.CoreUsersOptions, compare bool, outputFormat string) error {
	if compare && outputFormat != "raw" && outputFormat != "yaml" && outputFormat != "markdown" {
		return errors.New("-o must be one of: raw, yaml, markdown")
	}
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
	if compare && receipt.CoreUsersDiff != nil {
		if outputErr := writeCoreUsersDiff(cmd, outputFormat, receipt); outputErr != nil {
			return outputErr
		}
	}
	if err != nil {
		return err
	}
	if compare {
		if !receipt.FullBusinessEqual {
			return errors.New("core user email, password hash or Proxy UUID differs; see safe diff output")
		}
		return nil
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
}

func writeCoreUsersDiff(cmd *cobra.Command, outputFormat string, receipt migrate.FullBusinessReceipt) error {
	switch outputFormat {
	case "raw":
		return json.NewEncoder(cmd.OutOrStdout()).Encode(receipt)
	case "yaml":
		jsonBytes, err := json.Marshal(receipt)
		if err != nil {
			return errors.New("could not encode safe diff output")
		}
		var jsonValue any
		if err := json.Unmarshal(jsonBytes, &jsonValue); err != nil {
			return errors.New("could not encode safe diff output")
		}
		yamlBytes, err := yaml.Marshal(jsonValue)
		if err != nil {
			return errors.New("could not encode safe diff output")
		}
		_, err = cmd.OutOrStdout().Write(yamlBytes)
		return err
	case "markdown":
		diff := receipt.CoreUsersDiff
		if diff == nil {
			return errors.New("safe core-user diff summary is unavailable")
		}
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "# Core users diff\n\n- Result: `%s`\n- Source users: %d\n- Target users: %d\n- Equal: `%t`\n\n| Field | Mismatches |\n|---|---:|\n| Email | %d |\n| Proxy UUID | %d |\n| Password hash | %d |\n| Source-only users | %d |\n| Target-only users | %d |\n",
			receipt.Result, receipt.CoreUsers.Source.Count, receipt.CoreUsers.Target.Count, diff.Equal,
			diff.EmailMismatches, diff.ProxyUUIDMismatches, diff.PasswordHashMismatches, diff.SourceOnlyUsers, diff.TargetOnlyUsers)
		return err
	default:
		return errors.New("-o must be one of: raw, yaml, markdown")
	}
}
