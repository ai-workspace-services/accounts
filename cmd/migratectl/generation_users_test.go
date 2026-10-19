package main

import "testing"

func TestGenerationUserCommandsAcceptDSNEnvironmentNamesOnly(t *testing.T) {
	root := newRootCmd()
	var syncUser bool
	for _, command := range root.Commands() {
		if command.Use == "sync-user" {
			syncUser = true
			if command.Flags().Lookup("source-dsn-env") == nil || command.Flags().Lookup("target-dsn-env") == nil {
				t.Fatal("sync-user must accept DSN environment variable names")
			}
			if command.Flags().Lookup("source-dsn") != nil || command.Flags().Lookup("target-dsn") != nil {
				t.Fatal("sync-user must not accept connection strings on the command line")
			}
			if command.Flags().Lookup("apply") == nil || command.Flags().Lookup("expected-plan-sha256") == nil || command.Flags().Lookup("source-writers-paused") == nil {
				t.Fatal("sync-user is missing explicit apply safeguards")
			}
		}
	}
	if !syncUser {
		t.Fatal("sync-user command is not registered")
	}
}
