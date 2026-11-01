package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func openRegistrationRequest(router *gin.Engine, email, name string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"email": email, "name": name, "password": "testpass123"})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestOpenRegistrationActiveWithoutChangingLegacyUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	ctx := context.Background()
	old := &store.User{Name: "Legacy", Email: "legacy@example.com", EmailVerified: false, Groups: []string{"User"}, Active: true, MFAEnabled: true}
	if err := st.CreateUser(ctx, old); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetUserByID(ctx, old.ID)
	spy := &createUserSpy{Store: st}
	router := gin.New()
	RegisterRoutes(router, WithStore(spy), WithEmailVerification(true), WithRegistrationMode("open"))
	rec := openRegistrationRequest(router, "new@example.com", "NewUser")
	if rec.Code != 201 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if len(spy.created) != 1 || !spy.created[0].Active {
		t.Fatal("registration must persist active=true")
	}
	user, err := st.GetUserByEmail(ctx, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !user.Active || user.EmailVerified || !store.AccountAccessVerified(user) {
		t.Fatal("new account must allow ordinary access without asserting email ownership")
	}
	token := loginFor(t, router, user.Email, "testpass123")
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("new account session: %d %s", rec.Code, rec.Body.String())
	}
	after, _ := st.GetUserByID(ctx, old.ID)
	if !reflect.DeepEqual(before, after) || store.AccountAccessVerified(after) {
		t.Fatal("legacy account changed or gained an email bypass")
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/auth/registration-policy", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"emailVerificationRequired":false`) || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("wrong public policy: %s", rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/api/auth/register/send", strings.NewReader(`{"email":"new@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "email_verification_disabled") {
		t.Fatal("open registration must not send registration codes")
	}
}

func TestOpenRegistrationConcurrentDuplicateEmail(t *testing.T) {
	st := store.NewMemoryStore()
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithRegistrationMode("open"))
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for i, email := range []string{" NEW@example.com ", "new@example.com"} {
		wg.Add(1)
		go func(i int, email string) {
			defer wg.Done()
			rec := openRegistrationRequest(router, email, fmt.Sprintf("NewUser%d", i))
			if rec.Code != 201 && (rec.Code != 409 || !strings.Contains(rec.Body.String(), "email_already_exists")) {
				t.Errorf("unexpected response: %d %s", rec.Code, rec.Body.String())
			}
			results <- rec.Code
		}(i, email)
	}
	wg.Wait()
	close(results)
	created, conflict := 0, 0
	for status := range results {
		if status == 201 {
			created++
		}
		if status == 409 {
			conflict++
		}
	}
	if created != 1 || conflict != 1 {
		t.Fatalf("created=%d conflict=%d", created, conflict)
	}
	if rec := openRegistrationRequest(router, "New@example.com", "ThirdUser"); rec.Code != 409 {
		t.Fatalf("repeated registration must conflict: %d", rec.Code)
	}
	users, err := st.ListUsers(context.Background())
	if err != nil || len(users) != 1 {
		t.Fatal("duplicate registration created extra users")
	}
}

func TestOpenRegistrationRetainsRateLimitAndEmailMode(t *testing.T) {
	router := gin.New()
	st := store.NewMemoryStore()
	RegisterRoutes(router, WithStore(st), WithRegistrationMode("open"))
	// Name collisions consume the existing email probe budget without creating accounts.
	for i := 0; i < registrationProbeEmailLimit; i++ {
		openRegistrationRequest(router, "same@example.com", "SameUser")
	}
	if rec := openRegistrationRequest(router, "same@example.com", "AnotherUser"); rec.Code != 429 {
		t.Fatalf("expected registration limit, got %d", rec.Code)
	}
	emailRouter := gin.New()
	RegisterRoutes(emailRouter, WithStore(store.NewMemoryStore()), WithEmailVerification(true))
	if rec := openRegistrationRequest(emailRouter, "email@example.com", "EmailUser"); rec.Code != 400 || !strings.Contains(rec.Body.String(), "verification_required") {
		t.Fatalf("default email mode changed: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.GetUserByEmail(context.Background(), "email@example.com"); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatal("unexpected account")
	}
}
