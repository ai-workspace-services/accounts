package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

const xconnectProxyIdentityDomain = "xconnect.proxy_identity_ref.v1"

func xconnectObservabilitySearchAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		provided := strings.TrimSpace(c.GetHeader("X-XConnect-Observability-Token"))
		expected := strings.TrimSpace(os.Getenv("XCONNECT_OBSERVABILITY_SEARCH_TOKEN"))
		if expected == "" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "identity resolver unavailable"})
			return
		}
		if len(provided) != len(expected) || subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		c.Next()
	}
}

type resolveXConnectIdentityRequest struct {
	Email     string `json:"email"`
	ProxyUUID string `json:"proxyUuid"`
}

// resolveXConnectTelemetryIdentity accepts raw identifiers only over the
// authenticated internal POST route and returns a credential-keyed pseudonym.
// Neither the request body nor the raw identity is written to logs or returned.
func (h *handler) resolveXConnectTelemetryIdentity(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	var request resolveXConnectIdentityRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		respondError(c, http.StatusBadRequest, "invalid_identity_query", "provide exactly one email or proxyUuid")
		return
	}
	email := strings.ToLower(strings.TrimSpace(request.Email))
	proxyUUID := strings.ToLower(strings.TrimSpace(request.ProxyUUID))
	if (email == "") == (proxyUUID == "") || len(email) > 320 || len(proxyUUID) > 128 {
		respondError(c, http.StatusBadRequest, "invalid_identity_query", "provide exactly one email or proxyUuid")
		return
	}
	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		respondError(c, http.StatusInternalServerError, "identity_lookup_failed", "identity lookup failed")
		return
	}
	for _, user := range users {
		matches := email != "" && strings.EqualFold(strings.TrimSpace(user.Email), email)
		matches = matches || proxyUUID != "" && strings.EqualFold(strings.TrimSpace(user.ProxyUUID), proxyUUID)
		if !matches || strings.TrimSpace(user.ProxyUUID) == "" {
			continue
		}
		c.JSON(http.StatusOK, gin.H{
			"found":              true,
			"proxyIdentityRef":   xconnectProxyIdentityRef(user.ProxyUUID),
			"identityRefVersion": 1,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"found": false, "identityRefVersion": 1})
}

// xconnectProxyIdentityRef derives the trace pseudonym from the high-entropy
// VLESS credential itself. Using the credential as the HMAC key means the
// Accounts service and authenticated Xray can derive the same ref without a
// fleet-wide telemetry key. Only the HMAC output is emitted to trace storage.
func xconnectProxyIdentityRef(proxyUUID string) string {
	credential := strings.ToLower(strings.TrimSpace(proxyUUID))
	mac := hmac.New(sha256.New, []byte(credential))
	_, _ = io.WriteString(mac, xconnectProxyIdentityDomain)
	return hex.EncodeToString(mac.Sum(nil))
}
