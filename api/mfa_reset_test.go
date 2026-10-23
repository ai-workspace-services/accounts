package api

import (
	"account/internal/store"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestMFAResetNeedsUserAuthorizationAndRootReview(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	secret := "JBSWY3DPEHPK3PXP"
	u := &store.User{Name: "Recovery", Email: "mfa-review@example.invalid", EmailVerified: true, Active: true, MFAEnabled: true, MFATOTPSecret: secret}
	root := &store.User{Name: "Root reviewer", Email: "root-review@example.invalid", EmailVerified: true, Active: true, Role: store.RoleRoot, MFAEnabled: true, MFATOTPSecret: secret}
	admin := &store.User{Name: "Non root", Email: "admin-review@example.invalid", EmailVerified: true, Active: true, Role: store.RoleAdmin, MFAEnabled: true, MFATOTPSecret: secret}
	for _, usr := range []*store.User{u, root, admin} {
		if err := st.CreateUser(ctx, usr); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, usr.ID, usr.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	mail := &testEmailSender{}
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailSender(mail))
	call := func(path, token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	for _, email := range []string{"missing@example.invalid", u.Email} {
		if w := call("/api/auth/mfa/recovery/request", "", fmt.Sprintf(`{"email":%q}`, email)); w.Code != 202 {
			t.Fatalf("request %d %s", w.Code, w.Body.String())
		}
	}
	msg, ok := mail.last()
	if !ok {
		t.Fatal("missing email")
	}
	code := regexp.MustCompile(`[0-9]{6}`).FindString(msg.PlainBody)
	if code == "" {
		t.Fatal("missing code")
	}
	body := fmt.Sprintf(`{"email":%q,"code":%q,"reason":"Lost authenticator device","authorized":false}`, u.Email, code)
	if w := call("/api/auth/mfa/recovery/confirm", "", body); w.Code != 400 {
		t.Fatal("missing authorization accepted")
	}
	body = strings.Replace(body, `"authorized":false`, `"authorized":true`, 1)
	if w := call("/api/auth/mfa/recovery/confirm", "", body); w.Code != 202 {
		t.Fatalf("confirm %d %s", w.Code, w.Body.String())
	}
	unchanged, _ := st.GetUserByID(ctx, u.ID)
	if !unchanged.MFAEnabled {
		t.Fatal("email confirmation disabled MFA")
	}
	requests, err := st.ListMFAResetRequests(ctx)
	if err != nil || len(requests) != 1 {
		t.Fatalf("queue %v %v", requests, err)
	}
	raw, _ := json.Marshal(requests)
	if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "codeHash") {
		t.Fatal("secret in queue")
	}
	path := "/api/auth/admin/mfa-recovery/" + requests[0].ID
	otp, _ := totp.GenerateCode(secret, time.Now())
	decision := fmt.Sprintf(`{"decision":"approved","reason":"Verified user identity and authorization","code":%q}`, otp)
	if w := call(path, admin.ID, decision); w.Code != 403 {
		t.Fatalf("non-root accepted: %d", w.Code)
	}
	if w := call(path, root.ID, strings.Replace(decision, otp, "invalid", 1)); w.Code != 403 {
		t.Fatalf("invalid root factor accepted: %d", w.Code)
	}
	if err := st.ReplaceMFARecoveryCodes(ctx, u.ID, []store.MFARecoveryCode{{ID: "old-code", UserID: u.ID, BatchID: "batch", CodeHash: "hash", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	if w := call(path, root.ID, decision); w.Code != 200 {
		t.Fatalf("approval %d %s", w.Code, w.Body.String())
	}
	after, _ := st.GetUserByID(ctx, u.ID)
	if after.MFAEnabled || after.MFATOTPSecret != "" {
		t.Fatal("factor not revoked")
	}
	if _, _, err := st.GetSession(ctx, u.ID); err == nil {
		t.Fatal("old session survived")
	}
	codes, _ := st.ListMFARecoveryCodes(ctx, u.ID, time.Now())
	if len(codes) != 0 {
		t.Fatal("old recovery codes survived")
	}
	if w := call(path, root.ID, decision); w.Code != 409 {
		t.Fatal("replayed approval succeeded")
	}
	logs, _ := st.ListAuditLogs(ctx, store.AuditLogFilter{ActionPrefix: "identity.mfa_recovery."})
	if len(logs) != 1 {
		t.Fatalf("expected one audit event: %d", len(logs))
	}
}

func TestPaymentDoesNotRequireMFA(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	st := store.NewMemoryStore()
	u := &store.User{Name: "No MFA", Email: "payment-no-mfa@example.invalid", Active: true, EmailVerified: true}
	if err := st.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	_ = st.CreateSession(ctx, "payment-session", u.ID, time.Now().Add(time.Hour))
	router := gin.New()
	RegisterRoutes(router, WithStore(st))
	for _, path := range []string{"/api/auth/stripe/checkout", "/api/auth/stripe/subscription/preview", "/api/auth/stripe/subscription/retry", "/api/auth/stripe/portal"} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		req.Header.Set("Authorization", "Bearer payment-session")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != 503 || !strings.Contains(w.Body.String(), "stripe_not_configured") {
			t.Fatalf("payment blocked before provider validation: %s %d %s", path, w.Code, w.Body.String())
		}
	}
}
