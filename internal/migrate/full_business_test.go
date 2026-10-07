package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"

	schema "account/sql"
)

func TestFullBusinessContractNativeCoverage(t *testing.T) {
	tables, order, err := fullBusinessContract()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 53 || len(optionalBusinessTables) != 9 {
		t.Fatal("reviewed full scope differs")
	}
	body, manifest, err := schema.NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)CREATE TABLE public\.(\w+) \(\n(.*?)\n\);`)
	columns := regexp.MustCompile(`(?m)^    (\w+) `)
	for _, match := range re.FindAllStringSubmatch(string(body), -1) {
		table, ok := tables[match[1]]
		if !ok {
			continue
		}
		var names []string
		for _, c := range columns.FindAllStringSubmatch(match[2], -1) {
			if c[1] != "CONSTRAINT" {
				names = append(names, c[1])
			}
		}
		if len(names) != len(table.Columns) {
			t.Fatalf("native column scope drift: %s", match[1])
		}
		for i, c := range table.Columns {
			if c.Name != names[i] {
				t.Fatal("native column order drift")
			}
		}
	}
	if len(manifest.BusinessTables) != 52 {
		t.Fatal("native scope drift")
	}
	positions := map[string]int{}
	for i, name := range order {
		positions[name] = i
	}
	for name, table := range tables {
		for _, fk := range table.ForeignKeys {
			if positions[fk.Parent] >= positions[name] {
				t.Fatal("foreign-key order incorrect")
			}
		}
	}
	tables["users"] = businessTable{ForeignKeys: []businessFK{{Parent: "billing_ledger"}}}
	if _, err = businessOrder(tables); err == nil {
		t.Fatal("cycle accepted")
	}
}

func TestFullBusinessCanonicalExactNumbers(t *testing.T) {
	a := rawRow{"amount": json.RawMessage(`9007199254740993`), "data": json.RawMessage(`{"b":1.20,"a":[true,null,"number"]}`)}
	b := rawRow{"data": json.RawMessage(`{"a":[true,null,"number"],"b":12e-1}`), "amount": json.RawMessage(`9007199254740993.0`)}
	x, e := canonicalBusinessJSON(a)
	if e != nil {
		t.Fatal(e)
	}
	y, e := canonicalBusinessJSON(b)
	if e != nil || !bytes.Equal(x, y) {
		t.Fatal("exact JSON numeric equality changed")
	}
	b["amount"] = json.RawMessage(`9007199254740992`)
	y, _ = canonicalBusinessJSON(b)
	if bytes.Equal(x, y) {
		t.Fatal("integer precision lost")
	}
	a["amount"] = json.RawMessage(`1`)
	b["amount"] = json.RawMessage(`"1"`)
	x, _ = canonicalBusinessJSON(a)
	y, _ = canonicalBusinessJSON(b)
	if bytes.Equal(x, y) {
		t.Fatal("numeric/string collision")
	}
}

func TestFullBusinessDigestOrderAndCoverage(t *testing.T) {
	table := businessTable{Columns: []businessColumn{{Name: "id", Type: "text"}, {Name: "amount", Type: "bigint"}, {Name: "at", Type: "timestamp with time zone"}}, PrimaryKey: []string{"id"}}
	rows := []rawRow{{"id": json.RawMessage(`"a"`), "amount": json.RawMessage(`9007199254740993`), "at": json.RawMessage(`"2026-10-07T08:00:00+08:00"`)}, {"id": json.RawMessage(`"b"`), "amount": json.RawMessage(`4`), "at": json.RawMessage(`"2026-10-07T00:00:00Z"`)}}
	a, b := businessDigest{}, businessDigest{}
	for _, r := range rows {
		if e := a.add(r, table); e != nil {
			t.Fatal(e)
		}
	}
	rows[0]["at"] = json.RawMessage(`"2026-10-07T00:00:00Z"`)
	for i := len(rows) - 1; i >= 0; i-- {
		if e := b.add(rows[i], table); e != nil {
			t.Fatal(e)
		}
	}
	x, e := a.finish()
	if e != nil {
		t.Fatal(e)
	}
	y, e := b.finish()
	if e != nil || x != y || x.Rows != 2 {
		t.Fatal("ordered canonical digest mismatch")
	}
	if len(a.Leaves) != 0 {
		t.Fatal("digest retained row hash memory")
	}
	a.add(rows[0], table)
	a.add(rows[0], table)
	if _, e = a.finish(); e == nil {
		t.Fatal("duplicate PK accepted")
	}
	delete(rows[0], "amount")
	if e = a.add(rows[0], table); e == nil {
		t.Fatal("missing field accepted")
	}
}

func TestFullBusinessEmailAndProxyAuthority(t *testing.T) {
	source := map[string]businessUser{"a@example.invalid": {ID: "source", Email: " A@example.invalid ", Proxy: "prod-proxy"}}
	target := map[string]businessUser{"a@example.invalid": {ID: "uat", Email: " A@example.invalid ", Proxy: "prod-proxy"}}
	m, e := businessUserMap(source, target)
	if e != nil || m["source"] != "uat" {
		t.Fatal("email matching UUID remap failed")
	}
	u := target["a@example.invalid"]
	u.Proxy = "uat-proxy"
	target["a@example.invalid"] = u
	if _, e = businessUserMap(source, target); e == nil {
		t.Fatal("Proxy mismatch accepted")
	}
}

func TestFullBusinessProjectionReferencesAndLatestFields(t *testing.T) {
	tables, _, _ := fullBusinessContract()
	mapping := map[string]string{"source": "uat"}
	for _, name := range []string{"finance_payments", "tenant_memberships", "overlay_registrations", "account_lifecycle_events", "audit_logs"} {
		table := tables[name]
		row := rawRow{}
		cols := []ColumnDefinition{}
		for _, c := range table.Columns {
			row[c.Name] = json.RawMessage("null")
			cols = append(cols, ColumnDefinition{Name: c.Name, Type: c.Type})
		}
		key := map[string]string{"finance_payments": "account_uuid", "tenant_memberships": "user_id", "overlay_registrations": "owner_user_id", "account_lifecycle_events": "actor_ref", "audit_logs": "actor_uuid"}[name]
		row[key] = json.RawMessage(`"source"`)
		if e := projectBusinessRow(name, row, cols, table, mapping); e != nil || rowString(row, key) != "uat" {
			t.Fatalf("semantic reference not remapped: %s %v", name, e)
		}
	}
	table := tables["users"]
	row := rawRow{}
	cols := []ColumnDefinition{}
	for _, c := range table.Columns {
		if strings.HasPrefix(c.Name, "account_lifecycle_") || absentNativeUserMetadata(c) {
			continue
		}
		row[c.Name] = json.RawMessage("null")
		cols = append(cols, ColumnDefinition{Name: c.Name, Type: c.Type})
	}
	row["uuid"] = json.RawMessage(`"source"`)
	row["proxy_uuid"] = json.RawMessage(`"prod-proxy"`)
	row["email_verified"] = json.RawMessage(`false`)
	if e := validateBusinessSourceColumns("users", cols, table); e != nil {
		t.Fatal(e)
	}
	if e := projectBusinessRow("users", row, cols, table, mapping); e != nil || rowString(row, "account_lifecycle_state") != "active" || rowString(row, "uuid") != "uat" || rowString(row, "proxy_uuid") != "prod-proxy" {
		t.Fatal("latest projection changed authoritative fields")
	}
	for _, key := range []string{"subscription_valid_from", "subscription_valid_until", "last_active_at", "archived_at"} {
		if string(row[key]) != "null" {
			t.Fatalf("absent new metadata %s must retain native NULL default", key)
		}
	}
	row["email_verified"] = json.RawMessage(`true`)
	if e := projectBusinessRow("users", row, append(cols, ColumnDefinition{}), table, mapping); e == nil {
		t.Fatal("inconsistent verification accepted")
	}
	cols = append(cols, ColumnDefinition{Name: "legacy_unreviewed", Type: "text"})
	if e := validateBusinessSourceColumns("users", cols, table); e == nil {
		t.Fatal("unknown source column silently omitted")
	}
}

func TestFullBusinessNewMetadataNeverOverridesExistingValues(t *testing.T) {
	tables, _, _ := fullBusinessContract()
	table := tables["users"]
	row := rawRow{}
	cols := []ColumnDefinition{}
	for _, c := range table.Columns {
		row[c.Name] = json.RawMessage("null")
		cols = append(cols, ColumnDefinition{Name: c.Name, Type: c.Type})
	}
	row["email_verified"] = json.RawMessage("false")
	row["subscription_valid_until"] = json.RawMessage(`"2027-01-01T00:00:00Z"`)
	if err := projectBusinessRow("users", row, cols, table, map[string]string{}); err != nil {
		t.Fatal(err)
	}
	if string(row["subscription_valid_until"]) != `"2027-01-01T00:00:00Z"` {
		t.Fatal("existing authoritative subscription expiry changed")
	}
	for _, c := range []businessColumn{
		{Name: "subscription_valid_until", Type: "text", Nullable: true},
		{Name: "subscription_valid_until", Type: "timestamp with time zone", Nullable: false},
		{Name: "proxy_uuid", Type: "timestamp with time zone", Nullable: true},
	} {
		if absentNativeUserMetadata(c) {
			t.Fatal("unreviewed omission accepted")
		}
	}
}

func TestFullBusinessInvalidContractsDoNotConnect(t *testing.T) {
	_, manifest, _ := schema.NativeArtifact()
	good := FullBusinessOptions{Environment: "prod", SchemaSHA256: manifest.SchemaSHA256, BillingSHA256: FullBusinessBillingSHA256, WritersPaused: true}
	cases := []FullBusinessOptions{good, good, good, good}
	cases[0].Environment = ""
	cases[1].SchemaSHA256 = "bad"
	cases[2].BillingSHA256 = "bad"
	cases[3].WritersPaused = false
	for _, o := range cases {
		_, e := CopyFullBusiness(context.Background(), "private://fixture-secret", "private://fixture-secret", o)
		if e == nil || strings.Contains(e.Error(), "fixture-secret") {
			t.Fatal("invalid or leaking contract")
		}
	}
	for _, src := range []string{"postgres://admin:fixture@localhost/source?sslmode=disable", "postgres://readonly_release:fixture@remote/source?sslmode=disable", "postgres://readonly_release:fixture@remote/source?sslmode=prefer"} {
		if e := validateBusinessConnections(src, "postgres://postgres:fixture@localhost/account?sslmode=disable"); e == nil {
			t.Fatal("unsafe source accepted")
		}
	}
	if e := validateBusinessConnections("postgres://postgres.projectref:fixture@localhost/source?sslmode=disable", "postgres://postgres:fixture@localhost/account?sslmode=disable"); e != nil {
		t.Fatal("existing Serverless session-pooler role rejected")
	}
	if e := validateBusinessConnections("postgres://readonly_release.projectref:fixture@localhost/source?sslmode=disable", "postgres://postgres:fixture@localhost/account?sslmode=disable"); e != nil {
		t.Fatal("pooler readonly role rejected before actual current_user proof")
	}
	if e := validateBusinessConnections("postgres://readonly_release:fixture@localhost/account?sslmode=disable", "postgres://postgres:fixture@localhost/account?sslmode=disable"); e == nil {
		t.Fatal("same database accepted")
	}
}

func TestFullBusinessFixtureBillingChecksum(t *testing.T) {
	body, e := os.ReadFile("testdata/cloud_vendor_costs_native.sql")
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if hash != FullBusinessBillingSHA256 {
		t.Fatal("Billing qualification fixture differs from reviewed owner SQL")
	}
}
