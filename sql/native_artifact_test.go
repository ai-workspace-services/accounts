package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestNativeArtifactMatchesCurrentSchemaSources(t *testing.T) {
	body, manifest, err := NativeArtifact()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range manifest.SourceFiles {
		raw, readErr := os.ReadFile(filepath.Join("..", source.Path))
		if readErr != nil {
			t.Fatal(readErr)
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != source.SHA256 {
			t.Fatalf("native snapshot must be qualified against current source %s", source.Path)
		}
	}
	actual := regexp.MustCompile(`(?m)^CREATE TABLE public\.([a-z_]+)`).FindAllStringSubmatch(string(body), -1)
	var tables []string
	for _, match := range actual {
		tables = append(tables, match[1])
	}
	sort.Strings(tables)
	if strings.Join(tables, ",") != strings.Join(manifest.BusinessTables, ",") {
		t.Fatal("native table declaration scope differs")
	}
	if strings.Contains(string(body), "maintain_email_verified") {
		t.Fatal("generated email_verified must not have a writable compatibility trigger")
	}
	for _, pattern := range []string{`(?m)^DROP `, `(?m)^INSERT `, `(?m)^UPDATE `, `(?m)^GRANT `, `(?m)^REVOKE `, `(?m)^SET (?:lock|statement)_timeout\s*=\s*0`, `(?m)^\\`} {
		if regexp.MustCompile(pattern).Match(body) {
			t.Fatalf("native SQL contains forbidden initialization statement %s", pattern)
		}
	}
}
