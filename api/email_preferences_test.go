package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func emailTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open("file:"+uuid.NewString()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.AutoMigrate(&model.AdminSetting{}, &model.EmailManagementSetting{}, &model.EmailPreference{}, &model.EmailPreferenceToken{}, &model.EmailPreferenceEvent{}); err != nil {
		t.Fatal(err)
	}
	service.SetDB(d)
	t.Cleanup(func() { service.SetDB(nil); sqlDB, _ := d.DB(); sqlDB.Close() })
	return d
}

func TestEmailManagementAndSelfService(t *testing.T) {
	gin.SetMode(gin.TestMode)
	d := emailTestDB(t)
	ctx := context.Background()
	st := store.NewMemoryStore()
	users := map[string]*store.User{}
	for _, role := range []string{store.RoleRoot, store.RoleAdmin, store.RoleOperator, store.RoleUser} {
		u := &store.User{ID: uuid.NewString(), Name: role, Email: role + "@example.com", Role: role, Active: true, EmailVerified: true}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		users[role] = u
		if err := st.CreateSession(ctx, "session-"+role, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	router := gin.New()
	RegisterRoutes(router, WithStore(st))
	request := func(method, path, session string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var b []byte
		if body != nil {
			b, _ = json.Marshal(body)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("Authorization", "Bearer session-"+session)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	settingsPath := "/api/auth/admin/email-management"
	for _, role := range []string{"", store.RoleUser, store.RoleAdmin, store.RoleOperator} {
		rec := request("GET", settingsPath, role, nil)
		if rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("role %q received settings: %d", role, rec.Code)
		}
	}
	s := service.EmailManagementSettings{CompanyName: "Example <LLC>", PostalAddress: "123 Example St", ContactEmail: "support@example.com", ConsoleURL: "https://console.example"}
	rec := request("PUT", settingsPath, store.RoleRoot, s)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = request("PUT", settingsPath, store.RoleRoot, s)
	if rec.Code != 409 {
		t.Fatal("stale settings accepted", rec.Code)
	}
	rec = request("POST", settingsPath+"/preview", store.RoleRoot, map[string]any{})
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var preview map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &preview)
	if !strings.Contains(preview["htmlBody"], "&lt;LLC&gt;") || strings.Contains(preview["htmlBody"], "回复") {
		t.Fatal("footer is unsafe or still asks for replies")
	}
	// No marketing preferences are selected by account creation.
	user := users[store.RoleUser]
	prefToken, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "preferences")
	if err != nil {
		t.Fatal(err)
	}
	unsubToken, err := service.IssueEmailPreferenceToken(ctx, user.ID, user.Email, "unsubscribe")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/auth/email-preferences?key=" + prefToken
	rec = request("GET", path, "", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var p service.EmailPreferences
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.ProductUpdates || p.Tutorials || p.Offers {
		t.Fatal("default opted into marketing")
	}
	p.ProductUpdates = true
	p.Offers = true
	rec = request("PUT", path, "", p)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	// Unsubscribe token cannot read or opt in through the preference endpoint.
	rec = request("PUT", "/api/auth/email-preferences?key="+unsubToken, "", p)
	if rec.Code != 401 {
		t.Fatal("token purpose bypass", rec.Code)
	}
	rec = request("GET", "/api/auth/email-unsubscribe?key="+unsubToken, "", nil)
	if rec.Code != 404 {
		t.Fatal("GET unsubscribe mutated state")
	}
	before, _ := service.GetEmailPreferences(ctx, user.ID)
	if !before.Offers {
		t.Fatal("GET revoked consent")
	}
	for range 2 {
		rec = request("POST", "/api/auth/email-unsubscribe?key="+unsubToken, "", nil)
		if rec.Code != 204 {
			t.Fatal(rec.Code, rec.Body.String())
		}
	}
	rec = request("PUT", path, "", p)
	if rec.Code != 409 {
		t.Fatal("stale preference form restored marketing", rec.Code)
	}
	after, _ := service.GetEmailPreferences(ctx, user.ID)
	if after.ProductUpdates || after.Offers {
		t.Fatal("unsubscribe did not persist")
	}
	// A new router (process restart) sees persisted preferences, not handler memory.
	replacement := gin.New()
	RegisterRoutes(replacement, WithStore(st))
	router = replacement
	rec = request("GET", path, "", nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"offers":false`) {
		t.Fatal("restart lost unsubscribe")
	}
	var count int64
	d.Model(&model.EmailPreferenceEvent{}).Where("user_id = ?", user.ID).Count(&count)
	if count != 3 {
		t.Fatal("missing consent evidence", count)
	}
	// Token expires and is tied to the original mailbox.
	d.Model(&model.EmailPreferenceToken{}).Where("purpose = ? AND user_id = ?", "preferences", user.ID).Update("expires_at", time.Now().UTC().Add(-time.Hour))
	rec = request("GET", path, "", nil)
	if rec.Code != 401 {
		t.Fatal("expired link accepted", rec.Code, rec.Body.String())
	}
	user.Email = "changed@example.com"
	if err := st.UpdateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	rec = request("POST", "/api/auth/email-unsubscribe?key="+unsubToken, "", nil)
	if rec.Code != 401 {
		t.Fatal("old mailbox token accepted")
	}
}

func TestMarketingConsentGateAndTransactionalIsolation(t *testing.T) {
	emailTestDB(t)
	ctx := context.Background()
	st := store.NewMemoryStore()
	user := &store.User{ID: uuid.NewString(), Name: "recipient", Email: "recipient@example.com", Active: true, EmailVerified: true, Role: store.RoleUser}
	if err := st.CreateUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	var delivered []EmailMessage
	h := handler{store: st, emailSender: EmailSenderFunc(func(_ context.Context, msg EmailMessage) error { delivered = append(delivered, msg); return nil })}
	s := service.EmailManagementSettings{CompanyName: "Example LLC", PostalAddress: "Example address", ContactEmail: "support@example.com", ConsoleURL: "https://console.example"}
	if _, err := service.SaveEmailManagementSettings(ctx, s, "root"); err != nil {
		t.Fatal(err)
	}
	msg := EmailMessage{To: []string{"someone-else@example.com"}, PlainBody: "Feature update", HTMLBody: "<p>Feature update</p>", Marketing: &MarketingRecipient{UserID: user.ID, Category: "productUpdates", MonthlyDigest: true}}
	if err := h.deliverEmail(ctx, msg); !errors.Is(err, ErrMarketingSuppressed) {
		t.Fatal("unsolicited mail allowed", err)
	}
	if _, err := service.SaveEmailPreferences(ctx, user.ID, service.EmailPreferences{ProductUpdates: true, Frequency: "monthly"}, "account_settings"); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverEmail(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || len(delivered[0].To) != 1 || delivered[0].To[0] != user.Email || delivered[0].UnsubscribeURL == "" || !strings.Contains(delivered[0].HTMLBody, "/email/preferences?key=") {
		t.Fatal("missing recipient footer or one-click header")
	}
	if err := service.UnsubscribeEmail(ctx, user.ID, "unsubscribe_link"); err != nil {
		t.Fatal(err)
	}
	if err := h.deliverEmail(ctx, msg); !errors.Is(err, ErrMarketingSuppressed) {
		t.Fatal("revoked consent ignored", err)
	}
	if err := h.deliverEmail(ctx, EmailMessage{To: []string{user.Email}, PlainBody: "Receipt"}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 || delivered[1].UnsubscribeURL != "" {
		t.Fatal("transactional mail affected by unsubscribe")
	}
}
