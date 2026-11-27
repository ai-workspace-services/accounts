package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
)

type homepageVideoTestEnv struct {
	router     *gin.Engine
	adminToken string
	userToken  string
}

func setupHomepageVideoTestRouter(t *testing.T) homepageVideoTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.AutoMigrate(&model.AdminSetting{}, &model.HomepageVideoSetting{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	service.SetDB(db)
	t.Cleanup(func() {
		service.SetDB(nil)
		sqlDB, _ := db.DB()
		sqlDB.Close()
	})

	memoryStore := store.NewMemoryStore()
	ctx := context.Background()

	createUser := func(name, email, password, role string, level int) string {
		hashed, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			t.Fatalf("hash password: %v", err)
		}
		user := &store.User{
			Name:          name,
			Email:         email,
			PasswordHash:  string(hashed),
			Role:          role,
			Level:         level,
			EmailVerified: true,
		}
		if err := memoryStore.CreateUser(ctx, user); err != nil {
			t.Fatalf("create user: %v", err)
		}
		return password
	}

	adminPassword := createUser("admin", "admin@example.com", "AdminPass123!", store.RoleAdmin, store.LevelAdmin)
	userPassword := createUser("user", "user@example.com", "UserPass123!", store.RoleUser, store.LevelUser)

	router := gin.New()
	RegisterRoutes(router, WithStore(memoryStore), WithEmailVerification(false))

	login := func(email, password string) string {
		payload := map[string]string{
			"email":    email,
			"password": password,
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp := httptest.NewRecorder()
		router.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("login failed for %s: %d %s", email, resp.Code, resp.Body.String())
		}
		var result struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
			t.Fatalf("decode login response: %v", err)
		}
		return result.Token
	}

	return homepageVideoTestEnv{
		router:     router,
		adminToken: login("admin@example.com", adminPassword),
		userToken:  login("user@example.com", userPassword),
	}
}

func TestHomepageVideoPublicRetired(t *testing.T) {
	env := setupHomepageVideoTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/homepage-video", nil)
	req.Header.Set("X-Forwarded-Host", "cn-www.svc.plus")
	resp := httptest.NewRecorder()
	env.router.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("expected status 404 after retirement, got %d (%s)", resp.Code, resp.Body.String())
	}
}

func TestHomepageVideoAdminReadWrite(t *testing.T) {
	env := setupHomepageVideoTestRouter(t)

	body, _ := json.Marshal(map[string]any{
		"defaultEntry": map[string]string{
			"videoUrl":  "https://www.youtube.com/watch?v=test-main",
			"posterUrl": "https://cdn.svc.plus/default-poster.png",
		},
		"overrides": []map[string]string{
			{
				"domain":    "demo.svc.plus",
				"videoUrl":  "https://www.bilibili.com/video/BV1demo",
				"posterUrl": "https://cdn.svc.plus/demo-poster.png",
			},
		},
	})

	req := httptest.NewRequest(http.MethodPut, "/api/auth/admin/homepage-video", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+env.adminToken)
	resp := httptest.NewRecorder()
	env.router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", resp.Code, resp.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/auth/admin/homepage-video", nil)
	req.Header.Set("Authorization", "Bearer "+env.adminToken)
	resp = httptest.NewRecorder()
	env.router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d (%s)", resp.Code, resp.Body.String())
	}

	var payload struct {
		DefaultEntry struct {
			VideoURL string `json:"videoUrl"`
		} `json:"defaultEntry"`
		Overrides []struct {
			Domain string `json:"domain"`
		} `json:"overrides"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.DefaultEntry.VideoURL != "https://www.youtube.com/watch?v=test-main" {
		t.Fatalf("unexpected default video url: %q", payload.DefaultEntry.VideoURL)
	}
	if len(payload.Overrides) != 1 || payload.Overrides[0].Domain != "demo.svc.plus" {
		t.Fatalf("unexpected overrides payload: %+v", payload.Overrides)
	}
}

func TestHomepageVideoAdminUnauthorized(t *testing.T) {
	env := setupHomepageVideoTestRouter(t)

	req := httptest.NewRequest(http.MethodGet, "/api/auth/admin/homepage-video", nil)
	req.Header.Set("Authorization", "Bearer "+env.userToken)
	resp := httptest.NewRecorder()
	env.router.ServeHTTP(resp, req)
	if resp.Code != http.StatusForbidden {
		t.Fatalf("expected status 403, got %d", resp.Code)
	}
}
