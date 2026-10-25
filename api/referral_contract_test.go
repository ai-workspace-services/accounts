package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"account/internal/auth"
	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
)

const referralContractPassword = "referral-contract-password"

type referralContractHarness struct {
	router *gin.Engine
	store  store.Store
}

func newReferralContractHarness(t *testing.T) *referralContractHarness {
	t.Helper()
	gin.SetMode(gin.TestMode)
	st := store.NewMemoryStore()
	ts := auth.NewTokenService(auth.TokenConfig{
		PublicToken:   "referral-public",
		AccessSecret:  "referral-access-secret",
		RefreshSecret: "referral-refresh-secret",
		AccessExpiry:  time.Hour,
		RefreshExpiry: time.Hour,
		Store:         st,
	})
	router := gin.New()
	RegisterRoutes(router, WithStore(st), WithEmailVerification(false), WithTokenService(ts))
	return &referralContractHarness{router: router, store: st}
}

func (h *referralContractHarness) user(t *testing.T, id, name, email, role string, permissions ...string) (*store.User, string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(referralContractPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user := &store.User{
		ID: id, Name: name, Email: email, PasswordHash: string(hash),
		EmailVerified: true, Active: true, Role: role, Permissions: permissions,
	}
	if err := h.store.CreateUser(context.Background(), user); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	if user.Email == "" {
		return user, ""
	}
	return user, loginFor(t, h.router, user.Email, referralContractPassword)
}

func referralContractRequest(t *testing.T, router http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func seedReferralReward(t *testing.T, st store.Store, referrer, invitee *store.User, email string) *store.ReferralReward {
	t.Helper()
	code, err := st.EnsureReferralCode(context.Background(), referrer.ID, "REF"+strings.ToUpper(strings.ReplaceAll(referrer.ID, "-", "")))
	if err != nil {
		t.Fatalf("ensure referral code: %v", err)
	}
	if err := st.CreateReferralAttribution(context.Background(), code.Code, invitee.ID, time.Now().UTC().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("create referral attribution: %v", err)
	}
	reward := &store.ReferralReward{
		ProviderInvoiceID: "in_referral_contract_" + invitee.ID,
		ReferrerUUID:      referrer.ID,
		InviteeUUID:       invitee.ID,
		InviteeEmail:      email,
		PlanID:            "PRO-MONTHLY",
		ProductSlug:       "xconnect",
		PaidMinor:         1000,
		RewardMinor:       100,
		Currency:          "USD",
		PaidAt:            time.Now().UTC().Add(-14 * 24 * time.Hour),
	}
	if _, err := st.CreateReferralReward(context.Background(), reward); err != nil {
		t.Fatalf("create referral reward: %v", err)
	}
	return reward
}

func TestReferralOverviewRequiresAuthentication(t *testing.T) {
	h := newReferralContractHarness(t)
	rec := referralContractRequest(t, h.router, http.MethodGet, "/api/auth/referrals", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET referrals status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestReferralOverviewIsOwnerScopedAndMasksInvitee(t *testing.T) {
	h := newReferralContractHarness(t)
	owner, token := h.user(t, "ref-owner", "ref owner", "owner@example.test", store.RoleUser)
	otherOwner, _ := h.user(t, "other-owner", "other owner", "other-owner@example.test", store.RoleUser)
	invitee, _ := h.user(t, "ref-invitee", "ref invitee", "", store.RoleUser)
	otherInvitee, _ := h.user(t, "other-invitee", "other invitee", "", store.RoleUser)
	const inviteeEmail = "邀请好友@example.net"
	const otherEmail = "private-other@example.org"
	seedReferralReward(t, h.store, owner, invitee, inviteeEmail)
	seedReferralReward(t, h.store, otherOwner, otherInvitee, otherEmail)

	rec := referralContractRequest(t, h.router, http.MethodGet, "/api/auth/referrals", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET referrals status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Referrals []struct {
			Invitee string `json:"invitee"`
		} `json:"referrals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode referral response: %v", err)
	}
	if len(payload.Referrals) != 1 {
		t.Fatalf("owner received %d referrals, want only their own one: %s", len(payload.Referrals), rec.Body.String())
	}
	masked := payload.Referrals[0].Invitee
	if masked == inviteeEmail || strings.Contains(rec.Body.String(), inviteeEmail) {
		t.Fatalf("invitee email is not masked: field=%q response=%s", masked, rec.Body.String())
	}
	if strings.Contains(masked, "\uFFFD") {
		t.Fatalf("masking split the invitee's UTF-8 local part: %q", masked)
	}
	if strings.Contains(rec.Body.String(), otherEmail) || strings.Contains(rec.Body.String(), otherInvitee.ID) {
		t.Fatalf("owner response contains another user's referral data: %s", rec.Body.String())
	}
}

func TestReferralSettlementRejectsMalformedCurrencyAndOversizedBody(t *testing.T) {
	h := newReferralContractHarness(t)
	_, token := h.user(t, "settlement-owner", "settlement owner", "settlement@example.test", store.RoleUser)

	malformed := referralContractRequest(t, h.router, http.MethodPost, "/api/auth/referrals/settlements", token, []byte(`{"currency":"!!!"}`))
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed currency status = %d, want 400: %s", malformed.Code, malformed.Body.String())
	}

	oversizedJSON := []byte(`{"currency":"USD","padding":"` + strings.Repeat("x", 20*1024) + `"}`)
	oversized := referralContractRequest(t, h.router, http.MethodPost, "/api/auth/referrals/settlements", token, oversizedJSON)
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized settlement body status = %d, want 413: %s", oversized.Code, oversized.Body.String())
	}
	var errorPayload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(oversized.Body.Bytes(), &errorPayload); err != nil || errorPayload.Error != "request_too_large" {
		t.Fatalf("oversized settlement error = %q, decode err=%v body=%s", errorPayload.Error, err, oversized.Body.String())
	}
}

func TestReferralRegistrationRejectsOversizedBody(t *testing.T) {
	h := newReferralContractHarness(t)
	body := []byte(fmt.Sprintf(`{"name":"new user","email":"new-referral@example.test","password":%q,"padding":"%s"}`,
		referralContractPassword, strings.Repeat("x", 20*1024)))
	rec := referralContractRequest(t, h.router, http.MethodPost, "/api/auth/register", "", body)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized registration body status = %d, want 413: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Error != "request_too_large" {
		t.Fatalf("oversized registration error = %q, decode err=%v body=%s", payload.Error, err, rec.Body.String())
	}
}

func TestReferralRegistrationRejectsInvalidCodeBeforeCreatingAccount(t *testing.T) {
	h := newReferralContractHarness(t)
	body := []byte(`{"name":"invalid referral","email":"invalid-referral@example.test","password":"referral-contract-password","referralCode":"bad!"}`)
	rec := referralContractRequest(t, h.router, http.MethodPost, "/api/auth/register", "", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid referral code status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Error != "invalid_referral_code" {
		t.Fatalf("invalid referral code error = %q, decode err=%v body=%s", payload.Error, err, rec.Body.String())
	}
	if _, err := h.store.GetUserByEmail(context.Background(), "invalid-referral@example.test"); !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("invalid referral code created an account, lookup err=%v", err)
	}
}

func TestReferralAdminSettlementRequiresMoneyPermission(t *testing.T) {
	service.SetDB(nil)
	t.Cleanup(func() { service.SetDB(nil) })
	h := newReferralContractHarness(t)
	operator, deniedToken := h.user(t, "ref-operator-denied", "ref operator denied", "operator-denied@example.test", store.RoleOperator)
	_ = operator
	if rec := referralContractRequest(t, h.router, http.MethodPost, "/api/auth/admin/referrals/settlements/not-a-settlement/complete", deniedToken,
		[]byte(`{"payoutReference":"payout-1","reason":"reviewed"}`)); rec.Code != http.StatusForbidden {
		t.Fatalf("operator without money permission status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	db, err := gorm.Open(sqlite.Open("file:referral_permission_contract?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open permission db: %v", err)
	}
	if err := db.AutoMigrate(&model.AdminSetting{}); err != nil {
		t.Fatalf("migrate permission db: %v", err)
	}
	service.SetDB(db)
	_, err = service.SaveAdminSettings(context.Background(), service.AdminSettings{
		Matrix: map[string]map[string]bool{
			permissionAdminBillingMoneyWrite: {store.RoleOperator: true},
		},
	})
	if err != nil {
		t.Fatalf("grant operator money permission: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get permission sql db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	operator, allowedToken := h.user(t, "ref-operator-allowed", "ref operator allowed", "operator-allowed@example.test", store.RoleOperator)
	owner, _ := h.user(t, "ref-settlement-owner", "ref settlement owner", "ref-owner@example.test", store.RoleUser)
	invitee, _ := h.user(t, "ref-settlement-invitee", "ref settlement invitee", "ref-invitee@example.test", store.RoleUser)
	reward := seedReferralReward(t, h.store, owner, invitee, invitee.Email)
	if _, err := h.store.RequestReferralSettlement(context.Background(), owner.ID, reward.Currency, time.Now().UTC()); err != nil {
		t.Fatalf("request settlement: %v", err)
	}
	settlements, err := h.store.ListReferralSettlements(context.Background(), owner.ID, -1)
	if err != nil || len(settlements) != 1 {
		t.Fatalf("load settlement: count=%d err=%v", len(settlements), err)
	}
	path := "/api/auth/admin/referrals/settlements/" + settlements[0].ID + "/complete"
	allowed := referralContractRequest(t, h.router, http.MethodPost, path, allowedToken,
		[]byte(`{"payoutReference":"payout-referral-contract","reason":"verified payment"}`))
	if allowed.Code != http.StatusOK {
		t.Fatalf("operator with money permission status = %d, want 200: %s", allowed.Code, allowed.Body.String())
	}
	_ = operator
}

func TestReferralAdminReconcilePaidRequiresAdminAndIsIdempotentlyAudited(t *testing.T) {
	service.SetDB(nil)
	t.Cleanup(func() { service.SetDB(nil) })
	h := newReferralContractHarness(t)
	_, deniedToken := h.user(t, "ref-reconcile-operator", "reconcile operator", "reconcile-operator@example.test", store.RoleOperator)
	admin, adminToken := h.user(t, "ref-reconcile-admin", "reconcile admin", "reconcile-admin@example.test", store.RoleAdmin)
	owner, _ := h.user(t, "ref-reconcile-owner", "reconcile owner", "reconcile-owner@example.test", store.RoleUser)
	invitee, _ := h.user(t, "ref-reconcile-invitee", "reconcile invitee", "reconcile-invitee@example.test", store.RoleUser)
	reward := seedReferralReward(t, h.store, owner, invitee, invitee.Email)
	if _, err := h.store.RequestReferralSettlement(context.Background(), owner.ID, reward.Currency, time.Now().UTC()); err != nil {
		t.Fatalf("request settlement: %v", err)
	}
	settlements, err := h.store.ListReferralSettlements(context.Background(), owner.ID, -1)
	if err != nil || len(settlements) != 1 {
		t.Fatalf("load settlement: count=%d err=%v", len(settlements), err)
	}
	settlementID := settlements[0].ID
	if err := h.store.RecordReferralRisk(context.Background(), store.ReferralPaymentRisk{
		SourceKey: "charge.refunded:" + reward.ProviderInvoiceID, ProviderInvoiceID: reward.ProviderInvoiceID,
		Type: "refund", Reason: "refund arrived after off-platform payout", OccurredAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("reject settlement on risk event: %v", err)
	}
	if _, err := h.store.RejectReferralSettlement(context.Background(), settlementID, admin.ID, "refund risk requires review", time.Now().UTC()); err != nil {
		t.Fatalf("reject risk-held settlement: %v", err)
	}
	settlements, err = h.store.ListReferralSettlements(context.Background(), owner.ID, -1)
	if err != nil || len(settlements) != 1 || settlements[0].Status != "rejected" {
		t.Fatalf("risk event did not reject settlement: settlements=%+v err=%v", settlements, err)
	}
	path := "/api/auth/admin/referrals/settlements/" + settlementID + "/reconcile-paid"
	denied := referralContractRequest(t, h.router, http.MethodPost, path, deniedToken,
		[]byte(`{"payoutReference":"actual-payout-123","reason":"confirmed prior payout"}`))
	if denied.Code != http.StatusForbidden {
		t.Fatalf("operator reconcile status = %d, want 403: %s", denied.Code, denied.Body.String())
	}
	invalid := referralContractRequest(t, h.router, http.MethodPost, path, adminToken,
		[]byte(`{"payoutReference":"actual-payout-123"}`))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("missing reconciliation reason status = %d, want 400: %s", invalid.Code, invalid.Body.String())
	}
	body := []byte(`{"payoutReference":"actual-payout-123","reason":"confirmed prior payout"}`)
	first := referralContractRequest(t, h.router, http.MethodPost, path, adminToken, body)
	if first.Code != http.StatusOK {
		t.Fatalf("admin reconciliation status = %d, want 200: %s", first.Code, first.Body.String())
	}
	second := referralContractRequest(t, h.router, http.MethodPost, path, adminToken, body)
	if second.Code != http.StatusOK {
		t.Fatalf("same-reference reconciliation replay status = %d, want 200: %s", second.Code, second.Body.String())
	}
	var firstResult, secondResult struct {
		Settlement store.ReferralSettlement `json:"settlement"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstResult); err != nil {
		t.Fatalf("decode first reconciliation: %v", err)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &secondResult); err != nil {
		t.Fatalf("decode replay reconciliation: %v", err)
	}
	if firstResult.Settlement.ID != settlementID || secondResult.Settlement.ID != settlementID || firstResult.Settlement.Status != "paid" || secondResult.Settlement.Status != "paid" || firstResult.Settlement.PayoutReference != "actual-payout-123" || secondResult.Settlement.PayoutReference != "actual-payout-123" {
		t.Fatalf("reconciliation replay changed payment fact: first=%+v second=%+v", firstResult.Settlement, secondResult.Settlement)
	}
	settlements, err = h.store.ListReferralSettlements(context.Background(), owner.ID, -1)
	if err != nil || len(settlements) != 1 || settlements[0].Status != "paid" || settlements[0].PayoutReference != "actual-payout-123" {
		t.Fatalf("reconciliation created or changed settlement state: settlements=%+v err=%v", settlements, err)
	}
	audit, err := h.store.ListAuditLogs(context.Background(), store.AuditLogFilter{ActionPrefix: "referral.settlement.reconcile_paid"})
	if err != nil || len(audit) != 2 {
		t.Fatalf("reconciliation action audit count = %d, want both request and replay audited: err=%v", len(audit), err)
	}
	for _, entry := range audit {
		after, _ := entry.Details["after"].(map[string]any)
		if entry.ActorUUID != admin.ID || entry.Details["settlement_id"] != settlementID || entry.Details["payout_reference"] != "actual-payout-123" || after["reconciliation"] != true {
			t.Errorf("audit did not capture admin, settlement, real payment reference, and reconciliation: %+v", entry)
		}
	}
}

func TestReferralInactiveAndExpiredSessionsAreRejected(t *testing.T) {
	h := newReferralContractHarness(t)
	suspended, suspendedToken := h.user(t, "ref-suspended", "ref suspended", "suspended@example.test", store.RoleUser)
	suspended.Active = false
	if err := h.store.UpdateUser(context.Background(), suspended); err != nil {
		t.Fatalf("suspend user: %v", err)
	}
	if rec := referralContractRequest(t, h.router, http.MethodGet, "/api/auth/referrals", suspendedToken, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("suspended user status = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	expiredUser, _ := h.user(t, "ref-expired", "ref expired", "expired@example.test", store.RoleUser)
	expiredToken := "expired-session-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := h.store.CreateSession(context.Background(), expiredToken, expiredUser.ID, time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("create expired session: %v", err)
	}
	if rec := referralContractRequest(t, h.router, http.MethodGet, "/api/auth/referrals", expiredToken, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

func TestReferralCampaignWindowBoundaries(t *testing.T) {
	campaign, err := store.NewMemoryStore().(store.ReferralCampaignStore).GetReferralCampaign(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if campaign.StartsAt == nil || campaign.EndsAt == nil {
		t.Fatal("default campaign requires editable initial dates")
	}
	if !campaign.Active(*campaign.StartsAt) {
		t.Fatal("campaign start instant must be inclusive")
	}
	if campaign.Active(campaign.StartsAt.Add(-time.Nanosecond)) {
		t.Fatal("instant before campaign start must be inactive")
	}
	if !campaign.Active(campaign.EndsAt.Add(-time.Nanosecond)) {
		t.Fatal("instant before exclusive end must remain active")
	}
	if campaign.Active(*campaign.EndsAt) {
		t.Fatal("campaign end instant must be exclusive")
	}
}
