package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

// Migration authorization uses the configured internal service credential.
func (h *handler) authorizeMigration(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	header := c.GetHeader("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		c.Status(http.StatusUnauthorized)
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	expected := strings.TrimSpace(os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if token == "" || expected == "" || subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
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
	operator := fmt.Sprintf("%x", sha256.Sum256([]byte("accounts-migration-internal-service")))
	c.JSON(http.StatusOK, gin.H{"operator_id": operator, "scope": request.Scope})
}
