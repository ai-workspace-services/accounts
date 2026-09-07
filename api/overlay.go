package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/overlay"
)

func (h *handler) registerOverlayRoutes(r *gin.Engine) {
	if h.overlayService == nil {
		return
	}
	overlayHTTP := overlay.NewHTTPHandler(h.overlayService)
	group := r.Group("/api/overlay/v1")
	group.POST("/join-tokens/exchange", overlayHTTP.Exchange)
	group.POST("/device/session", overlayHTTP.MintSession)
	group.GET("/enrollment/signed-config", overlayHTTP.EnrollmentConfig)
	group.GET("/gateway/signed-config", overlayHTTP.GatewayConfig)
	group.POST("/enrollment/signed-config/:generation/ack", overlayHTTP.AckEnrollment)
	group.GET("/enrollment/policy-artifacts/:generation/:digest", overlayHTTP.PolicyArtifact)

	// User-scoped routes require the normal Accounts JWT/session middleware.
	// They are registered only when the deployment has a token service; the
	// invite and device lifecycle remains independently authenticated.
	if h.tokenService == nil {
		return
	}
	userGroup := r.Group("/api/overlay/v1")
	userGroup.Use(h.tokenService.AuthMiddleware())
	userGroup.Use(auth.RequireActiveUser(h.store))
	userGroup.GET("/signing-keys", overlayHTTP.SigningKeys)
	userGroup.GET("/signed-config", func(c *gin.Context) {
		userID := auth.GetUserID(c)
		if userID == "" || userID == "system" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		overlayHTTP.UserConfig(c, userID)
	})
	userGroup.POST("/signed-config/:generation/ack", func(c *gin.Context) {
		userID := auth.GetUserID(c)
		if userID == "" || userID == "system" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		overlayHTTP.AckUser(c, userID)
	})
}
