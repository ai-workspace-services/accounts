package migrate

import (
	"context"
	"os"
	"regexp"
	"testing"
)

func TestResetFailsClosedWithoutConnecting(t *testing.T) {
	err := NewRunner("").Reset(context.Background(), "postgres://must-not-connect")
	if err == nil || !regexp.MustCompile(`(?i)disabled|forward-only`).MatchString(err.Error()) {
		t.Fatalf("Reset() should reject destructive reset without connecting, got %v", err)
	}
}

func TestCanonicalSchemaDoesNotDropTablesOrSchemas(t *testing.T) {
	schema, err := os.ReadFile("../../sql/schema.sql")
	if err != nil {
		t.Fatalf("read canonical schema: %v", err)
	}
	forbidden := regexp.MustCompile(`(?im)^\s*DROP\s+(TABLE|SCHEMA)\b`)
	if match := forbidden.Find(schema); match != nil {
		t.Fatalf("canonical schema must not drop existing data structures: %s", match)
	}
}
