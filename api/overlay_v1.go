package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"account/internal/auth"
	"account/internal/overlay"
)

func (h *handler) registerOverlayV1Routes(r *gin.Engine) {
	if h.overlayService == nil {
		return
	}
	overlayHTTP := overlay.NewHTTPHandler(h.overlayService)
	group := r.Group("/api/overlay/v1")
	group.POST("/registrations", overlayHTTP.Register)
	group.POST("/registrations/:registrationID/exchange", overlayHTTP.RegistrationExchange)
	group.POST("/join-tokens/exchange", overlayHTTP.Exchange)
	group.POST("/device/session", overlayHTTP.MintSession)
	group.GET("/enrollment/signed-config", overlayHTTP.EnrollmentConfig)
	group.GET("/gateway/signed-config", overlayHTTP.GatewayConfig)
	group.POST("/enrollment/signed-config/:generation/ack", overlayHTTP.AckEnrollment)
	group.GET("/enrollment/policy-artifacts/:generation/:digest", overlayHTTP.PolicyArtifact)
	if h.tokenService == nil {
		return
	}
	userGroup := r.Group("/api/overlay/v1")
	userGroup.Use(h.tokenService.AuthMiddleware())
	userGroup.Use(auth.RequireActiveUser(h.store))
	userGroup.GET("/signing-keys", overlayHTTP.SigningKeys)
	userGroup.GET("/gateways", func(c *gin.Context) {
		userID := auth.GetUserID(c)
		if userID == "" || userID == "system" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		overlayHTTP.UserGateways(c, userID)
	})
	userGroup.POST("/devices/enroll", func(c *gin.Context) {
		userID := auth.GetUserID(c)
		if userID == "" || userID == "system" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "user authentication required"})
			return
		}
		overlayHTTP.UserEnrollDevice(c, userID)
	})
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
