package api

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"account/internal/auth"
	"account/internal/store"
	"github.com/gin-gonic/gin"
)

// Authorize through the same Accounts authentication service and current user
// state. A service token, cookie-only request or unprivileged login is insufficient.
func (h *handler) authorizeMigration(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if !strings.HasPrefix(c.GetHeader("Authorization"), "Bearer ") || strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")) == "" {
		c.Status(http.StatusUnauthorized)
		return
	}
	var request struct {
		Scope string `json:"scope"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1024)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF || (request.Scope != "source.read" && request.Scope != "target.plan" && request.Scope != "target.apply") {
		c.Status(http.StatusBadRequest)
		return
	}
	var userID string
	if h.tokenService != nil {
		h.tokenService.AuthMiddleware()(c)
		if c.IsAborted() {
			return
		}
		userID = auth.GetUserID(c)
	} else {
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		id, expires, err := h.store.GetSession(c.Request.Context(), token)
		if err != nil || !time.Now().Before(expires) {
			c.Status(http.StatusUnauthorized)
			return
		}
		userID = id
	}
	user, err := h.store.GetUserByID(c.Request.Context(), userID)
	if err != nil {
		c.Status(http.StatusUnauthorized)
		return
	}
	if !user.Active || h.isReadOnlyAccount(user) || (!store.IsRootRole(user.Role) && !strings.EqualFold(strings.TrimSpace(user.Role), store.RoleAdmin)) {
		c.Status(http.StatusForbidden)
		return
	}
	// Audit ownership is local to each Accounts authority. Source and target
	// may authorize different administrators; the target plan keeps its owner.
	operator := fmt.Sprintf("%x", sha256.Sum256([]byte("accounts-migration-admin:"+user.ID)))
	c.JSON(http.StatusOK, gin.H{"operator_id": operator, "scope": request.Scope})
}
