package main

import (
	"bytes"
	"strings"
	"testing"

	"account/internal/migrate"
)

func TestDiffCommandRejectsDSNValuesAndWriteFlags(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"diff"})
	if err != nil || cmd.Name() != "diff" {
		t.Fatalf("diff subcommand is not registered: %v", err)
	}
	for _, flag := range []string{"source-dsn", "target-dsn", "dsn", "apply"} {
		t.Run(flag, func(t *testing.T) {
			var output bytes.Buffer
			cmd := newDiffCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetOut(&output)
			cmd.SetErr(&output)
			cmd.SetArgs([]string{"--" + flag, "postgres://fixture:private-fixture@127.0.0.1/account"})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "unknown flag") {
				t.Fatalf("unsafe flag was accepted: %v", err)
			}
			if strings.Contains(output.String()+err.Error(), "private-fixture") {
				t.Fatal("rejected credential value was echoed")
			}
		})
	}
}

func TestDiffCommandValidatesBeforeDatabaseAccess(t *testing.T) {
	t.Setenv("DIFF_TEST_SOURCE", "postgres://fixture:source-private@127.0.0.1:1/source")
	t.Setenv("DIFF_TEST_TARGET", "postgres://fixture:target-private@127.0.0.1:1/account")
	t.Setenv("DIFF_TEST_EMPTY", "")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"missing names", nil, "distinct --source-dsn-env"},
		{"same name", []string{"--source-dsn-env", "DIFF_TEST_SOURCE", "--target-dsn-env", "DIFF_TEST_SOURCE"}, "distinct --source-dsn-env"},
		{"empty value", []string{"--source-dsn-env", "DIFF_TEST_SOURCE", "--target-dsn-env", "DIFF_TEST_EMPTY"}, "environment variable is empty"},
		{"output", []string{"-o", "invalid"}, "-o must be one of"},
		{"reviewed guards", []string{"--source-dsn-env", "DIFF_TEST_SOURCE", "--target-dsn-env", "DIFF_TEST_TARGET"}, "reviewed Accounts schema hash"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newDiffCmd()
			cmd.SilenceErrors, cmd.SilenceUsage = true, true
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected validation %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "-private") {
				t.Fatal("validation exposed a DSN value")
			}
		})
	}
}

func TestDiffHelpAndOutputExcludeCredentialValues(t *testing.T) {
	const secret = "private-credential-fixture"
	t.Setenv("MIGRATECTL_SOURCE_DSN", "postgres://fixture:"+secret+"@localhost/source")
	var help bytes.Buffer
	cmd := newDiffCmd()
	cmd.SetOut(&help)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(help.String(), secret) || !strings.Contains(help.String(), "--source-dsn-env") {
		t.Fatal("help must describe environment names without their values")
	}
	receipt := migrate.FullBusinessReceipt{
		Result: "different", SourceReadOnly: true, TargetWrites: false,
		CoreUsers:     migrate.CoreUsersEvidence{Source: migrate.CoreUsersEquality{Count: 2}, Target: migrate.CoreUsersEquality{Count: 3}},
		CoreUsersDiff: &migrate.CoreUsersDiff{ProxyUUIDMismatches: 1, TargetOnlyUsers: 1},
	}
	for _, format := range []string{"raw", "yaml", "markdown"} {
		t.Run(format, func(t *testing.T) {
			var output bytes.Buffer
			cmd := newDiffCmd()
			cmd.SetOut(&output)
			if err := writeCoreUsersDiff(cmd, format, receipt); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(output.String(), secret) || !strings.Contains(output.String(), "different") {
				t.Fatal("safe diff must report the comparison without credentials")
			}
		})
	}
}
