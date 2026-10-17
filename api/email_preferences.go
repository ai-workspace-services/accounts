package api

import (
	"errors"
	"html"
	"net/http"
	"strings"

	"account/internal/service"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func emailPreferencePrivacy(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("X-Robots-Tag", "noindex, nofollow")
	c.Next()
}

func emailPreferenceError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	message := "email preferences unavailable"
	if errors.Is(err, service.ErrServiceDBNotInitialized) {
		status = http.StatusServiceUnavailable
	}
	if errors.Is(err, service.ErrEmailPreferenceConflict) {
		status = http.StatusConflict
		message = err.Error()
	}
	if errors.Is(err, service.ErrInvalidEmailPreferenceToken) {
		status = http.StatusUnauthorized
		message = err.Error()
	}
	c.JSON(status, gin.H{"error": message})
}
func (h *handler) requireEmailRoot(c *gin.Context) (*store.User, bool) {
	user, ok := h.requireAdminPermission(c, permissionAdminSettingsRead)
	if !ok {
		return nil, false
	}
	if !h.isRootAccount(user) || h.isReadOnlyAccount(user) {
		respondError(c, http.StatusForbidden, "root_only", "root only")
		return nil, false
	}
	return user, true
}
func (h *handler) getEmailManagement(c *gin.Context) {
	if _, ok := h.requireEmailRoot(c); !ok {
		return
	}
	s, err := service.GetEmailManagementSettings(c.Request.Context())
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	c.JSON(http.StatusOK, s)
}
func (h *handler) putEmailManagement(c *gin.Context) {
	user, ok := h.requireEmailRoot(c)
	if !ok {
		return
	}
	var s service.EmailManagementSettings
	if c.ShouldBindJSON(&s) != nil {
		respondError(c, 400, "invalid_request", "invalid settings")
		return
	}
	s, err := service.ValidateEmailManagementSettings(s)
	if err != nil {
		respondError(c, 400, "invalid_settings", err.Error())
		return
	}
	s, err = service.SaveEmailManagementSettings(c.Request.Context(), s, user.ID)
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	c.JSON(200, s)
}
func (h *handler) emailPreferenceIdentity(c *gin.Context, purpose string) (*store.User, bool) {
	token := c.Query("key")
	if token == "" {
		return h.requireAuthenticatedUser(c)
	}
	record, err := service.ResolveEmailPreferenceToken(c.Request.Context(), token, purpose)
	if err != nil {
		emailPreferenceError(c, err)
		return nil, false
	}
	user, err := h.store.GetUserByID(c.Request.Context(), record.UserID)
	if err != nil || !strings.EqualFold(strings.TrimSpace(user.Email), record.Email) {
		emailPreferenceError(c, service.ErrInvalidEmailPreferenceToken)
		return nil, false
	}
	return user, true
}
func (h *handler) getEmailPreferences(c *gin.Context) {
	user, ok := h.emailPreferenceIdentity(c, "preferences")
	if !ok {
		return
	}
	p, err := service.GetEmailPreferences(c.Request.Context(), user.ID)
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	c.JSON(200, p)
}
func (h *handler) putEmailPreferences(c *gin.Context) {
	user, ok := h.emailPreferenceIdentity(c, "preferences")
	if !ok {
		return
	}
	if h.isReadOnlyAccount(user) {
		respondError(c, 403, "read_only_account", "read-only account")
		return
	}
	var p service.EmailPreferences
	if c.ShouldBindJSON(&p) != nil || (p.Frequency != "monthly" && p.Frequency != "important") {
		respondError(c, 400, "invalid_request", "invalid preferences")
		return
	}
	source := "account_settings"
	if c.Query("key") != "" {
		source = "email_preference_link"
	}
	p, err := service.SaveEmailPreferences(c.Request.Context(), user.ID, p, source)
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	c.JSON(200, p)
}
func (h *handler) unsubscribeEmail(c *gin.Context) {
	user, ok := h.emailPreferenceIdentity(c, "unsubscribe")
	if !ok {
		return
	}
	if err := service.UnsubscribeEmail(c.Request.Context(), user.ID, "unsubscribe_link"); err != nil {
		emailPreferenceError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

// The preview creates links for the current root only. It never sends mail or
// grants Root a way to opt another user into marketing.
func (h *handler) previewMarketingFooter(c *gin.Context) {
	user, ok := h.requireEmailRoot(c)
	if !ok {
		return
	}
	s, err := service.GetEmailManagementSettings(c.Request.Context())
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	if _, err = service.ValidateEmailManagementSettings(s); err != nil {
		respondError(c, 400, "settings_required", "save sender identity first")
		return
	}
	pref, err := service.IssueEmailPreferenceToken(c.Request.Context(), user.ID, user.Email, "preferences")
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	unsub, err := service.IssueEmailPreferenceToken(c.Request.Context(), user.ID, user.Email, "unsubscribe")
	if err != nil {
		emailPreferenceError(c, err)
		return
	}
	prefURL := s.ConsoleURL + "/email/preferences?key=" + pref
	unsubURL := s.ConsoleURL + "/email/unsubscribe?key=" + unsub
	oneClickURL := s.ConsoleURL + "/api/auth/email-unsubscribe?key=" + unsub
	plain, body := renderMarketingFooter(s, prefURL, unsubURL)
	c.JSON(200, gin.H{"plainBody": plain, "htmlBody": body, "preferencesUrl": prefURL, "unsubscribeUrl": unsubURL, "oneClickUrl": oneClickURL})
}
func renderMarketingFooter(s service.EmailManagementSettings, prefURL, unsubURL string) (string, string) {
	reason := "你收到此邮件，是因为你已订阅相应类别的产品更新与优惠。"
	notice := "退订不会取消套餐；必要的账户、安全与账单通知仍会发送。"
	identity := s.CompanyName + " · " + s.PostalAddress + " · " + s.ContactEmail
	plain := reason + "\n管理邮件偏好：" + prefURL + "\n取消全部营销邮件：" + unsubURL + "\n" + notice + "\n" + identity
	body := "<footer><p>" + reason + "</p><p><a href=\"" + html.EscapeString(prefURL) + "\">管理邮件偏好</a> · <a href=\"" + html.EscapeString(unsubURL) + "\">取消全部营销邮件</a></p><p>" + notice + "</p><p>" + html.EscapeString(identity) + "</p></footer>"
	return plain, body
}
