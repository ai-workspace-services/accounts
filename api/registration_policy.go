package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func (h *handler) registrationPolicy(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	mode := "email"
	if h.registrationMode == "open" {
		mode = "open"
	}
	c.JSON(http.StatusOK, gin.H{"mode": mode, "emailVerificationRequired": mode != "open" && h.emailVerificationEnabled})
}
