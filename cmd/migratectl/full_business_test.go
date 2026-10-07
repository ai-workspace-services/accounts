package main

import (
	"strings"
	"testing"
)

func TestFullBusinessCLIUsesPrivateEnvironmentAndPreview(t *testing.T) {
	for _, compare := range []bool{false, true} {
		cmd := newFullBusinessCmd(compare)
		if value, _ := cmd.Flags().GetBool("dry-run"); !value {
			t.Fatal("preview must be default")
		}
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "dsn-env") {
			t.Fatal("missing private environment contract accepted")
		}
		if cmd.Flags().Lookup("source-dsn") != nil || cmd.Flags().Lookup("target-dsn") != nil {
			t.Fatal("DSN command-line value exposed")
		}
	}
}

func TestCoreUsersCLIUsesPrivateEnvironment(t *testing.T) {
	for _, compare := range []bool{false, true} {
		cmd := newCoreUsersCmd(compare)
		cmd.SetArgs([]string{})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "dsn-env") {
			t.Fatal("missing private environment contract accepted")
		}
		if cmd.Flags().Lookup("source-dsn") != nil || cmd.Flags().Lookup("target-dsn") != nil {
			t.Fatal("DSN command-line value exposed")
		}
	}
}
