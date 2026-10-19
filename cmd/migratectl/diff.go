package main

import "github.com/spf13/cobra"

// Keep every identity, checkpoint and read-only guard of compare-core-users.
func newDiffCmd() *cobra.Command {
	cmd := newCoreUsersCmd(true)
	cmd.Use = "diff"
	cmd.Short = "Compare core account identities read-only without exposing credentials"
	cmd.Long = "Compare normalized email matching, password hashes and Proxy UUIDs using " +
		"the same reviewed checkpoint and readonly source contract as compare-core-users. " +
		"Pass environment variable names with --source-dsn-env and --target-dsn-env; " +
		"connection strings are never accepted as command-line flags. Differences return a nonzero exit status."
	return cmd
}
