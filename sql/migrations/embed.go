package migrations

import "embed"

// Files contains the reviewed, versioned SQL migrations shipped with Accounts.
//
//go:embed *.up.sql
var Files embed.FS
