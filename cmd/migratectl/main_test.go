package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	migratelib "github.com/golang-migrate/migrate/v4"
)

func TestVersionRegistersFileSourceDriver(t *testing.T) {
	_, err := migratelib.New(fmt.Sprintf("file://%s", filepath.ToSlash(t.TempDir())), "unknown://")
	if err != nil {
		if strings.Contains(err.Error(), "unknown driver 'file'") {
			t.Fatalf("file source driver was not registered: %v", err)
		}
	}
}
