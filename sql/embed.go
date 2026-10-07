package schema

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed schema.sql
var schemaFile []byte

//go:embed init/accounts-native.sql init/accounts-native.manifest.json migrations/*.up.sql
var nativeFiles embed.FS

// NativeManifest describes the reviewed, row-free initialization artifact.
type NativeManifest struct {
	Format             int      `json:"format"`
	MigrationVersion   uint     `json:"migration_version"`
	SchemaSHA256       string   `json:"schema_sha256"`
	BusinessTableCount int      `json:"business_table_count"`
	BusinessTables     []string `json:"business_tables"`
	SourceFiles        []struct {
		Path   string `json:"path"`
		SHA256 string `json:"sha256"`
	} `json:"source_files"`
}

// NativeArtifact verifies the compiled SQL and its complete migration boundary.
// New migrations must qualify a new native snapshot before empty DB deployment.
func NativeArtifact() ([]byte, NativeManifest, error) {
	var manifest NativeManifest
	raw, err := nativeFiles.ReadFile("init/accounts-native.manifest.json")
	if err != nil {
		return nil, manifest, err
	}
	if err = json.Unmarshal(raw, &manifest); err != nil {
		return nil, manifest, err
	}
	sql, err := nativeFiles.ReadFile("init/accounts-native.sql")
	if err != nil {
		return nil, manifest, err
	}
	sum := sha256.Sum256(sql)
	if manifest.Format != 1 || manifest.SchemaSHA256 != hex.EncodeToString(sum[:]) ||
		manifest.BusinessTableCount != 52 || len(manifest.BusinessTables) != manifest.BusinessTableCount {
		return nil, manifest, fmt.Errorf("native schema manifest or SQL checksum differs")
	}
	expected := make(map[string]string)
	for _, source := range manifest.SourceFiles {
		if strings.HasPrefix(source.Path, "sql/migrations/") {
			expected[strings.TrimPrefix(source.Path, "sql/")] = source.SHA256
		}
	}
	files, err := nativeFiles.ReadDir("migrations")
	if err != nil {
		return nil, manifest, err
	}
	var latest uint
	if len(files) != len(expected) {
		return nil, manifest, fmt.Errorf("native schema migration scope differs")
	}
	for _, file := range files {
		name := path.Join("migrations", file.Name())
		body, readErr := nativeFiles.ReadFile(name)
		if readErr != nil {
			return nil, manifest, readErr
		}
		version, parseErr := strconv.ParseUint(strings.SplitN(file.Name(), "_", 2)[0], 10, 32)
		checksum := sha256.Sum256(body)
		if parseErr != nil || expected[name] != hex.EncodeToString(checksum[:]) {
			return nil, manifest, fmt.Errorf("native schema migration checksum differs")
		}
		if uint(version) > latest {
			latest = uint(version)
		}
	}
	if latest == 0 || latest != manifest.MigrationVersion {
		return nil, manifest, fmt.Errorf("native schema migration version differs")
	}
	return sql, manifest, nil
}

var (
	hashOnce sync.Once
	hash     string
)

// Hash returns the SHA-256 hash of the canonical schema.sql file.
func Hash() string {
	hashOnce.Do(func() {
		sum := sha256.Sum256(schemaFile)
		hash = hex.EncodeToString(sum[:])
	})
	return hash
}
