package model

import (
	"sync"
	"testing"

	"gorm.io/gorm/schema"
)

func TestTenantDomainUsesColumnUniqueConstraint(t *testing.T) {
	t.Parallel()

	parsed, err := schema.Parse(&TenantDomain{}, &sync.Map{}, schema.NamingStrategy{})
	if err != nil {
		t.Fatalf("parse TenantDomain schema: %v", err)
	}

	field := parsed.FieldsByDBName["domain"]
	if field == nil {
		t.Fatal("domain field is missing from TenantDomain schema")
	}
	if !field.Unique {
		t.Fatal("domain must be a column-level UNIQUE constraint so AutoMigrate does not drop a generated constraint name")
	}
}
