package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/auth"
	"account/internal/overlay"
	"account/internal/store"
)

var updateGolden = flag.Bool("update", false, "update golden test files")

type RouteEntry struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

func TestOverlayV1RoutesInventory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open("file:api-overlay-routes?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	service, err := overlay.NewService(db, overlay.Config{SigningPrivateKey: signer})
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore()
	tokenService := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "public-token",
		AccessSecret:  "access-secret",
		RefreshSecret: "refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})

	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithOverlayService(service), WithTokenService(tokenService))

	var overlayRoutes []RouteEntry
	for _, route := range router.Routes() {
		if strings.HasPrefix(route.Path, "/api/overlay/v1") {
			overlayRoutes = append(overlayRoutes, RouteEntry{
				Method: route.Method,
				Path:   route.Path,
			})
		}
	}

	sort.Slice(overlayRoutes, func(i, j int) bool {
		if overlayRoutes[i].Path == overlayRoutes[j].Path {
			return overlayRoutes[i].Method < overlayRoutes[j].Method
		}
		return overlayRoutes[i].Path < overlayRoutes[j].Path
	})

	goldenPath := filepath.Join("testdata", "overlay_v1_routes.golden.json")

	if *updateGolden {
		if err := os.MkdirAll("testdata", 0755); err != nil {
			t.Fatalf("failed to create testdata dir: %v", err)
		}
		data, err := json.MarshalIndent(overlayRoutes, "", "  ")
		if err != nil {
			t.Fatalf("failed to marshal routes: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(goldenPath, data, 0644); err != nil {
			t.Fatalf("failed to write golden file: %v", err)
		}
		t.Logf("updated golden file %s with %d routes", goldenPath, len(overlayRoutes))
		return
	}

	expectedRaw, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("golden file %s not found (run with -update to generate): %v", goldenPath, err)
	}

	var expectedRoutes []RouteEntry
	if err := json.Unmarshal(expectedRaw, &expectedRoutes); err != nil {
		t.Fatalf("failed to parse golden file %s: %v", goldenPath, err)
	}

	actualRaw, _ := json.MarshalIndent(overlayRoutes, "", "  ")
	expectedNormalized, _ := json.MarshalIndent(expectedRoutes, "", "  ")

	if string(actualRaw) != string(expectedNormalized) {
		t.Fatalf("overlay v1 routes mismatch with golden file:\nExpected:\n%s\nActual:\n%s", string(expectedNormalized), string(actualRaw))
	}
}
