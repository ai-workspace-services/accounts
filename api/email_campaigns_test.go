package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"account/internal/model"
	"account/internal/service"
	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type campaignEnv struct {
	db     *gorm.DB
	st     store.Store
	h      *handler
	router *gin.Engine
	root   *store.User
	user   *store.User
	sent   []EmailMessage
}

func campaignTestEnv(t *testing.T) *campaignEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	d := emailTestDB(t)
	st := store.NewMemoryStore()
	ctx := context.Background()
	env := &campaignEnv{db: d, st: st}
	makeUser := func(role string) *store.User {
		u := &store.User{ID: uuid.NewString(), Name: role, Email: role + "@example.com", Role: role, Level: store.LevelUser, Active: true, EmailVerified: true}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateSession(ctx, "test-"+role, u.ID, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		return u
	}
	env.root = makeUser(store.RoleRoot)
	makeUser(store.RoleAdmin)
	env.user = makeUser(store.RoleUser)
	sender := EmailSenderFunc(func(_ context.Context, msg EmailMessage) error { env.sent = append(env.sent, msg); return nil })
	env.h = &handler{store: st, db: d, emailSender: sender, emailSenderConfigured: true}
	env.router = gin.New()
	RegisterRoutes(env.router, WithStore(st), WithGormDB(d), WithEmailSender(sender), WithEmailVerification(false))
	_, err := service.SaveEmailManagementSettings(ctx, service.EmailManagementSettings{CompanyName: "Example LLC", PostalAddress: "Example street", ContactEmail: "support@example.com", ConsoleURL: "https://console.example"}, env.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	return env
}
func (e *campaignEnv) request(t *testing.T, method, path, role string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(method, "/api/auth/admin/"+path, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer test-"+role)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	return rec
}
func (e *campaignEnv) consent(t *testing.T) {
	t.Helper()
	if _, err := service.SaveEmailPreferences(context.Background(), e.user.ID, service.EmailPreferences{ProductUpdates: true, Tutorials: true, Frequency: "important"}, "account_settings"); err != nil {
		t.Fatal(err)
	}
}
func (e *campaignEnv) queue(t *testing.T) model.EmailDelivery {
	t.Helper()
	ctx := context.Background()
	c, err := service.CreateEmailCampaign(ctx, model.EmailCampaign{Kind: "feature", Subject: "New feature", Body: "Use the new feature", Audience: "all", DeliveryMode: "important"}, e.root.ID)
	if err != nil {
		t.Fatal(err)
	}
	row := newEmailDelivery(*e.user, c.ID, "", c.Subject, c.Body, "", "", "productUpdates", "important", "all", time.Now().UTC())
	if err = service.QueueEmailCampaign(ctx, c, []model.EmailDelivery{row}); err != nil {
		t.Fatal(err)
	}
	return row
}
func TestCampaignRootPreviewQueueAndUnsubscribeBeforeSend(t *testing.T) {
	e := campaignTestEnv(t)
	ctx := context.Background()
	for _, role := range []string{store.RoleUser, store.RoleAdmin} {
		rec := e.request(t, "GET", "email-campaigns", role, nil)
		if rec.Code != 403 {
			t.Fatal("non-root campaign access", rec.Code)
		}
	}
	rec := e.request(t, "POST", "email-campaigns", store.RoleRoot, map[string]any{"kind": "app_release", "subject": "App 2.0", "body": "App update details", "audience": "all", "deliveryMode": "important"})
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	var campaign model.EmailCampaign
	_ = json.Unmarshal(rec.Body.Bytes(), &campaign)
	rec = e.request(t, "POST", "email-campaigns/"+campaign.ID+"/preview", store.RoleRoot, nil)
	var p struct {
		Version        uint64 `json:"version"`
		Recipients     int    `json:"recipients"`
		AudienceDigest string `json:"audienceDigest"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Recipients != 0 {
		t.Fatal("unconsented audience")
	}
	e.consent(t)
	rec = e.request(t, "POST", "email-campaigns/"+campaign.ID+"/preview", store.RoleRoot, nil)
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Recipients != 1 {
		t.Fatal("consented recipient missing", rec.Body.String())
	}
	path := "email-campaigns/" + campaign.ID + "/launch"
	rec = e.request(t, "POST", path, store.RoleRoot, p)
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	rec = e.request(t, "POST", path, store.RoleRoot, p)
	if rec.Code != 409 {
		t.Fatal("duplicate campaign queued")
	}
	if err := service.UnsubscribeEmail(ctx, e.user.ID, "unsubscribe_link"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 0 {
		t.Fatal("queued mail ignored unsubscribe")
	}
	stats, _ := service.EmailDeliveryStats(ctx, campaign.ID)
	if stats["suppressed"] != 1 {
		t.Fatal("missing suppressed outcome", stats)
	}
}
func TestCampaignSendAndSafeRetry(t *testing.T) {
	e := campaignTestEnv(t)
	e.consent(t)
	ctx := context.Background()
	row := e.queue(t)
	attempt := 0
	e.h.emailSender = EmailSenderFunc(func(_ context.Context, msg EmailMessage) error {
		attempt++
		if attempt == 1 {
			return &textproto.Error{Code: 451, Msg: "temporary rejection"}
		}
		e.sent = append(e.sent, msg)
		return nil
	})
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var loaded model.EmailDelivery
	e.db.First(&loaded, "id = ?", row.ID)
	if loaded.Status != "retry" || loaded.Attempts != 1 {
		t.Fatal("temporary rejection not queued for retry", loaded.Status)
	}
	e.db.Model(&model.EmailDelivery{}).Where("id = ?", row.ID).Update("due_at", time.Now().UTC().Add(-time.Minute))
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 || !strings.HasPrefix(e.sent[0].Subject, "AD:") || e.sent[0].UnsubscribeURL == "" || len(e.sent[0].To) != 1 {
		t.Fatal("missing individual marketing footer or classification")
	}
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 {
		t.Fatal("sent mail duplicated")
	}
	available, err := service.EmailFrequencyAvailable(ctx, e.user.ID, "important", time.Now().UTC())
	if err != nil || available {
		t.Fatal("weekly cap ignored", err)
	}
}
func TestCampaignUnknownSMTPOutcomeRequiresExplicitRetry(t *testing.T) {
	e := campaignTestEnv(t)
	e.consent(t)
	ctx := context.Background()
	row := e.queue(t)
	e.h.emailSender = EmailSenderFunc(func(context.Context, EmailMessage) error { return errors.New("connection closed after DATA") })
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var loaded model.EmailDelivery
	e.db.First(&loaded, "id = ?", row.ID)
	if loaded.Status != "uncertain" {
		t.Fatal("unknown SMTP was retried", loaded.Status)
	}
	if err := service.RetryEmailDelivery(ctx, row.ID, false); !errors.Is(err, service.ErrEmailQueueConflict) {
		t.Fatal("unknown result retried without acknowledgment", err)
	}
	if err := service.RetryEmailDelivery(ctx, row.ID, true); err != nil {
		t.Fatal(err)
	}
	e.h.emailSender = EmailSenderFunc(func(_ context.Context, msg EmailMessage) error { e.sent = append(e.sent, msg); return nil })
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 {
		t.Fatal("verified retry not processed")
	}
}
func TestRecipientLeaseSerializesCampaignsAndCrashIsUncertain(t *testing.T) {
	e := campaignTestEnv(t)
	e.consent(t)
	ctx := context.Background()
	e.queue(t)
	e.queue(t)
	now := time.Now().UTC()
	row, err := service.ClaimEmailDelivery(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.ClaimEmailDelivery(ctx, now); !errors.Is(err, service.ErrNoEmailDelivery) {
		t.Fatal("concurrent recipient send allowed", err)
	}
	if err = service.MarkEmailSending(ctx, row); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Minute)
	e.db.Model(&model.EmailDelivery{}).Where("id = ?", row.ID).Update("lease_until", past)
	e.db.Model(&model.EmailRecipientLease{}).Where("user_id = ?", row.UserID).Update("expires_at", past)
	if _, err = e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var loaded model.EmailDelivery
	e.db.First(&loaded, "id = ?", row.ID)
	if loaded.Status != "uncertain" || len(e.sent) != 0 {
		t.Fatal("interrupted SMTP send was replayed")
	}
}
func TestWelcomeRegistrationEnrollmentAndTutorialConsent(t *testing.T) {
	e := campaignTestEnv(t)
	ctx := context.Background()
	for _, id := range []string{"hello", "first_steps", "discover_more"} {
		delay := 24
		if id == "hello" {
			delay = 0
		}
		if err := service.SaveOnboardingStep(ctx, model.EmailOnboardingStep{ID: id, Enabled: true, Subject: "Hello {{name}}", Body: "Account setup guide", DelayHours: delay}, e.root.ID); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest("POST", "/api/auth/register", strings.NewReader(`{"name":"new-user","email":"new@example.com","password":"LocalTestOnly123!"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	u, err := e.st.GetUserByEmail(ctx, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err = e.h.enrollEmailOnboarding(ctx, u); err != nil {
		t.Fatal(err)
	}
	var count int64
	e.db.Model(&model.EmailDelivery{}).Where("user_id = ?", u.ID).Count(&count)
	if count != 3 {
		t.Fatal("onboarding enrollment duplicated or missing", count)
	}
	if _, err = e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 || e.sent[0].UnsubscribeURL != "" || strings.HasPrefix(e.sent[0].Subject, "AD:") {
		t.Fatal("welcome missing or treated as promotion")
	}
	e.db.Model(&model.EmailDelivery{}).Where("user_id = ? AND step_id <> ?", u.ID, "hello").Update("due_at", time.Now().UTC().Add(-time.Minute))
	if _, err = e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 1 {
		t.Fatal("tutorial sent without consent")
	}
	// Existing users created before activation do not receive the welcome sequence.
	if err = e.h.enrollEmailOnboarding(ctx, e.user); err != nil {
		t.Fatal(err)
	}
	e.db.Model(&model.EmailDelivery{}).Where("user_id = ?", e.user.ID).Count(&count)
	if count != 0 {
		t.Fatal("old account backfilled into onboarding")
	}
}
func TestQueueCancellationAndBounceSuppression(t *testing.T) {
	e := campaignTestEnv(t)
	e.consent(t)
	ctx := context.Background()
	row := e.queue(t)
	if err := service.CancelEmailCampaign(ctx, row.CampaignID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(e.sent) != 0 {
		t.Fatal("cancelled campaign sent")
	}
	if err := service.RetryEmailDelivery(ctx, row.ID, true); !errors.Is(err, service.ErrEmailQueueConflict) {
		t.Fatal("cancelled campaign retried")
	}
	row = e.queue(t)
	if err := service.PutEmailSuppression(ctx, e.user.Email, "complaint", e.root.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.h.dispatchEmailBatch(ctx, 10); err != nil {
		t.Fatal(err)
	}
	var loaded model.EmailDelivery
	e.db.First(&loaded, "id = ?", row.ID)
	if loaded.Status != "suppressed" || len(e.sent) != 0 {
		t.Fatal("complaint suppression ignored")
	}
}

func TestFinalSMTPBoundaryRejectsRevokedConsent(t *testing.T) {
	e := campaignTestEnv(t)
	e.consent(t)
	ctx := context.Background()
	e.queue(t)
	row, err := service.ClaimEmailDelivery(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	p, err := service.GetEmailPreferences(ctx, e.user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = service.UnsubscribeEmail(ctx, e.user.ID, "unsubscribe_link"); err != nil {
		t.Fatal(err)
	}
	if err = service.MarkEmailSending(ctx, row, p.Version); !errors.Is(err, service.ErrEmailQueueConflict) {
		t.Fatal("revoked consent crossed SMTP boundary", err)
	}
}

func TestResolveUnknownAcceptanceAndStopSequence(t *testing.T) {
	e := campaignTestEnv(t)
	ctx := context.Background()
	row := e.queue(t)
	e.db.Model(&model.EmailDelivery{}).Where("id = ?", row.ID).Update("status", "uncertain")
	if err := service.ConfirmEmailAcceptance(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	var loaded model.EmailDelivery
	e.db.First(&loaded, "id = ?", row.ID)
	if loaded.Status != "sent" || loaded.SentAt == nil || len(e.sent) != 0 {
		t.Fatal("acceptance resolution must not resend")
	}
	if err := service.ConfirmEmailAcceptance(ctx, row.ID); !errors.Is(err, service.ErrEmailQueueConflict) {
		t.Fatal("resolution must be conditional", err)
	}
	prior := model.EmailDelivery{ID: uuid.NewString(), DedupeKey: uuid.NewString(), UserID: e.user.ID, StepID: "hello", Status: "suppressed"}
	if err := e.db.Create(&prior).Error; err != nil {
		t.Fatal(err)
	}
	_, err := service.OnboardingPredecessorSent(ctx, model.EmailDelivery{UserID: e.user.ID, StepID: "first_steps"})
	if !errors.Is(err, service.ErrOnboardingStopped) {
		t.Fatal("stopped predecessor did not stop sequence", err)
	}
}
