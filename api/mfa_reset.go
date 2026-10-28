package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// Email possession permits an application for manual review, never a reset.
func (h *handler) requestMFAReset(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		Email string `json:"email"`
	}
	if hasQueryParameter(c, "email") || c.ShouldBindJSON(&req) != nil || !strings.Contains(req.Email, "@") || len(req.Email) > 254 {
		respondError(c, 400, "invalid_request", "email is required in the request body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !allowAuthProbe(c, h.passwordRecoveryByIP, h.passwordRecoveryByEmail, email) {
		return
	}
	accepted := func() { c.JSON(http.StatusAccepted, gin.H{"status": "verification_requested"}) }
	u, err := h.store.GetUserByEmail(c.Request.Context(), email)
	if err != nil || !u.Active || !u.EmailVerified || !u.MFAEnabled || h.isReadOnlyAccount(u) {
		accepted()
		return
	}
	code, err := h.newVerificationCode()
	if err != nil {
		respondError(c, 503, "recovery_unavailable", "recovery unavailable")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
	if err != nil {
		respondError(c, 503, "recovery_unavailable", "recovery unavailable")
		return
	}
	now := time.Now().UTC()
	r := &store.MFAResetRequest{ID: uuid.NewString(), UserID: u.ID, Email: email, Fingerprint: store.MFAFingerprint(u.MFATOTPSecret), CodeHash: string(hash), Status: "pending_email", CreatedAt: now, ExpiresAt: now.Add(30 * time.Minute)}
	if err = h.store.CreateMFAResetRequest(c.Request.Context(), r); err != nil {
		accepted()
		return
	}
	// Distinct subject/purpose from password-reset codes; never include a factor.
	plain := fmt.Sprintf("MFA recovery authorization code: %s\nValid for 30 minutes. This code only submits a request for Root administrator review. It does not disable MFA. If you did not request recovery, ignore this email.", code)
	if requestLocale(c) == "zh" {
		plain = fmt.Sprintf("MFA 恢复申请验证码：%s\n30 分钟内有效。此验证码仅用于授权提交 Root 管理员审核，不会直接解除 MFA。若非本人操作，请忽略此邮件。", code)
	}
	if err := h.emailSender.Send(c.Request.Context(), EmailMessage{To: []string{email}, Subject: "MFA recovery authorization", PlainBody: plain}); err != nil {
		slog.Warn("MFA recovery authorization email could not be delivered", "request_id", r.ID)
	}
	accepted()
}

func (h *handler) confirmMFAReset(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var req struct {
		Email      string `json:"email"`
		Code       string `json:"code"`
		Reason     string `json:"reason"`
		Authorized bool   `json:"authorized"`
	}
	if hasQueryParameter(c, "email", "code") || c.ShouldBindJSON(&req) != nil || !req.Authorized || len(strings.TrimSpace(req.Reason)) < 5 || len(req.Reason) > 1000 || len(req.Code) != 6 {
		respondError(c, 400, "invalid_request", "email code, reason and explicit authorization are required")
		return
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	if !allowAuthProbe(c, h.passwordRecoveryConfirmByIP, h.passwordRecoveryConfirmByEmail, email) {
		return
	}
	r, err := h.store.GetMFAResetRequestByEmail(c.Request.Context(), email)
	if err != nil || r.Status != "pending_email" || r.FailedAttempts >= 5 || !r.ExpiresAt.After(time.Now()) {
		respondError(c, 400, "invalid_code", "invalid or expired recovery code")
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(r.CodeHash), []byte(req.Code)) != nil {
		_ = h.store.FailMFAResetCode(c.Request.Context(), r.ID)
		respondError(c, 400, "invalid_code", "invalid or expired recovery code")
		return
	}
	if err = h.store.ConfirmMFAResetRequest(c.Request.Context(), r.ID, r.CodeHash, strings.TrimSpace(req.Reason), time.Now().UTC()); err != nil {
		respondError(c, 409, "recovery_conflict", "request changed; request a new code")
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"status": "pending_review"})
}

func (h *handler) requireMFAResetRoot(c *gin.Context) (*store.User, bool) {
	u, ok := h.requireAdminPermission(c, permissionAdminUsersRoleWrite)
	if !ok {
		return nil, false
	}
	if !h.isRootAccount(u) || h.isReadOnlyAccount(u) {
		respondError(c, 403, "root_only", "Root administrator required")
		return nil, false
	}
	return u, true
}
func (h *handler) adminListMFAResets(c *gin.Context) {
	if _, ok := h.requireMFAResetRoot(c); !ok {
		return
	}
	requests, err := h.store.ListMFAResetRequests(c.Request.Context())
	if err != nil {
		respondError(c, 503, "recovery_unavailable", "recovery queue unavailable")
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, gin.H{"requests": requests})
}
func (h *handler) adminDecideMFAReset(c *gin.Context) {
	actor, ok := h.requireMFAResetRoot(c)
	if !ok {
		return
	}
	var req struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Code     string `json:"code"`
	}
	if c.ShouldBindJSON(&req) != nil || (req.Decision != "approved" && req.Decision != "rejected") || len(strings.TrimSpace(req.Reason)) < 5 || len(req.Reason) > 1000 {
		respondError(c, 400, "invalid_request", "decision and review reason required")
		return
	}
	if _, err := uuid.Parse(c.Param("requestId")); err != nil {
		respondError(c, 400, "invalid_request", "invalid request ID")
		return
	}
	if !allowAuthProbe(c, h.passwordRecoveryConfirmByIP, h.passwordRecoveryConfirmByEmail, actor.ID) {
		return
	}
	if !actor.MFAEnabled || !verifyTOTPCode(actor.MFATOTPSecret, strings.TrimSpace(req.Code), time.Now().UTC()) {
		respondError(c, 403, "invalid_mfa_factor", "Root MFA verification required")
		return
	}
	r, err := h.store.GetMFAResetRequestByID(c.Request.Context(), c.Param("requestId"))
	if err != nil {
		respondError(c, 409, "recovery_conflict", "request unavailable")
		return
	}
	if err := h.store.DecideMFAResetRequest(c.Request.Context(), c.Param("requestId"), actor.ID, req.Decision, strings.TrimSpace(req.Reason), time.Now().UTC()); err != nil {
		respondError(c, 409, "recovery_conflict", "request expired, changed or already decided")
		return
	}
	// Pending login challenges also verify current MFA state. Remove local copies.
	if req.Decision == "approved" {
		h.removeMFAChallengesForUser(r.UserID)
	}
	body := "Your MFA recovery request was reviewed: " + req.Decision + ". If approved, sign in again; existing sessions and recovery codes have been revoked. Password recovery remains available on the account recovery page."
	if err := h.emailSender.Send(c.Request.Context(), EmailMessage{To: []string{r.Email}, Subject: "MFA recovery review result", PlainBody: body}); err != nil {
		slog.Warn("MFA recovery review notification failed", "request_id", r.ID)
	}
	c.JSON(200, gin.H{"status": req.Decision})
}
