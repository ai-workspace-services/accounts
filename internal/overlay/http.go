package overlay

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

type HTTPHandler struct{ Service *Service }

func NewHTTPHandler(service *Service) *HTTPHandler { return &HTTPHandler{Service: service} }

func (h *HTTPHandler) Exchange(c *gin.Context) {
	var request ExchangeRequest
	if !decodeJSON(c, &request) {
		return
	}
	response, err := h.Service.Exchange(c.Request.Context(), request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *HTTPHandler) Register(c *gin.Context) {
	noStore(c)
	var request RegistrationRequest
	if !decodeJSONLimit(c, &request, 64*1024) {
		return
	}
	response, err := h.Service.Register(c.Request.Context(), request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusCreated, response)
}

func (h *HTTPHandler) RegistrationExchange(c *gin.Context) {
	noStore(c)
	token, ok := bearer(c.GetHeader("Authorization"), "Bearer")
	if !ok {
		writeError(c, ErrInvalidRegistrationToken)
		return
	}
	response, pending, err := h.Service.ExchangeRegistration(c.Request.Context(), c.Param("registrationID"), token)
	if errors.Is(err, ErrRegistrationPending) {
		noStore(c)
		c.JSON(http.StatusAccepted, pending)
		return
	}
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *HTTPHandler) MintSession(c *gin.Context) {
	credential, ok := bearer(c.GetHeader("Authorization"), "Device")
	if !ok {
		writeError(c, ErrInvalidToken)
		return
	}
	var request DeviceSessionRequest
	if !decodeJSON(c, &request) {
		return
	}
	response, err := h.Service.MintSession(c.Request.Context(), credential, request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *HTTPHandler) EnrollmentConfig(c *gin.Context) {
	token, ok := bearer(c.GetHeader("Authorization"), "Bearer")
	if !ok {
		writeError(c, ErrInvalidToken)
		return
	}
	config, etag, err := h.Service.EnrollmentConfig(c.Request.Context(), token, c.Query("device_id"), c.Query("network_id"), wantsV2(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeSignedConfig(c, config, etag)
}

func (h *HTTPHandler) GatewayConfig(c *gin.Context) {
	token, ok := bearer(c.GetHeader("Authorization"), "Bearer")
	if !ok {
		writeError(c, ErrInvalidToken)
		return
	}
	config, etag, err := h.Service.GatewayConfig(c.Request.Context(), token)
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", etag)
	c.Data(http.StatusOK, "application/json", mustJSON(config))
}

func (h *HTTPHandler) AckEnrollment(c *gin.Context) {
	token, ok := bearer(c.GetHeader("Authorization"), "Bearer")
	if !ok {
		writeError(c, ErrInvalidToken)
		return
	}
	generation, err := strconv.ParseUint(c.Param("generation"), 10, 64)
	if err != nil || generation == 0 {
		writeError(c, ErrInvalidInput)
		return
	}
	var request SignedConfigAckRequest
	if !decodeJSON(c, &request) {
		return
	}
	request.Generation = generation
	response, err := h.Service.AckEnrollment(c.Request.Context(), token, request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *HTTPHandler) PolicyArtifact(c *gin.Context) {
	token, ok := bearer(c.GetHeader("Authorization"), "Bearer")
	if !ok {
		writeError(c, ErrInvalidToken)
		return
	}
	generation, err := strconv.ParseUint(c.Param("generation"), 10, 64)
	if err != nil || generation == 0 {
		writeError(c, ErrInvalidInput)
		return
	}
	raw, err := h.Service.PolicyArtifact(c.Request.Context(), token, generation, c.Param("digest"))
	if err != nil {
		writeError(c, err)
		return
	}
	c.Header("Content-Type", PolicyMediaType)
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, PolicyMediaType, raw)
}

func (h *HTTPHandler) SigningKeys(c *gin.Context) {
	keys := struct {
		Keys []SigningKey `json:"keys"`
	}{Keys: h.Service.SigningKeys(h.Service.now())}
	data, err := json.Marshal(keys)
	if err != nil {
		writeError(c, err)
		return
	}
	// The key ring is public but scoped to the authenticated caller by the
	// route registration in api; cache variation prevents cross-user reuse.
	sum := HashSecret(string(data))
	etag := `"` + sum + `"`
	c.Header("Cache-Control", "private, max-age=300")
	c.Header("Vary", "Authorization")
	c.Header("ETag", etag)
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Status(http.StatusNotModified)
		return
	}
	c.Data(http.StatusOK, "application/json", data)
}

func (h *HTTPHandler) UserConfig(c *gin.Context, userID string) {
	config, etag, err := h.Service.UserConfig(c.Request.Context(), userID, c.Query("device_id"), c.Query("network_id"), wantsV2(c))
	if err != nil {
		writeError(c, err)
		return
	}
	writeSignedConfig(c, config, etag)
}

func (h *HTTPHandler) AckUser(c *gin.Context, userID string) {
	generation, err := strconv.ParseUint(c.Param("generation"), 10, 64)
	if err != nil || generation == 0 {
		writeError(c, ErrInvalidInput)
		return
	}
	var request SignedConfigAckRequest
	if !decodeJSON(c, &request) {
		return
	}
	request.Generation = generation
	response, err := h.Service.AckUser(c.Request.Context(), userID, request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func (h *HTTPHandler) UserGateways(c *gin.Context, userID string) {
	noStore(c)
	gateways, err := h.Service.UserGateways(c.Request.Context(), userID)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, gin.H{"gateways": gateways})
}

func (h *HTTPHandler) UserEnrollDevice(c *gin.Context, userID string) {
	noStore(c)
	var request UserEnrollRequest
	if !decodeJSONLimit(c, &request, 64*1024) {
		return
	}
	response, err := h.Service.UserEnrollDevice(c.Request.Context(), userID, request)
	if err != nil {
		writeError(c, err)
		return
	}
	noStore(c)
	c.JSON(http.StatusOK, response)
}

func decodeJSON(c *gin.Context, target any) bool {
	return decodeJSONLimit(c, target, 1<<20)
}

func decodeJSONLimit(c *gin.Context, target any, limit int64) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(c, ErrInvalidInput)
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(c, ErrInvalidInput)
		return false
	}
	return true
}

func bearer(header, scheme string) (string, bool) {
	prefix := scheme + " "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	value := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return value, value != ""
}

func wantsV2(c *gin.Context) bool {
	return strings.Contains(c.GetHeader("Accept"), SignedConfigV2MediaType)
}

func writeSignedConfig(c *gin.Context, config SignedConfig, etag string) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("ETag", etag)
	if config.SchemaVersion == 2 {
		c.Header("Vary", "Accept")
		c.Data(http.StatusOK, SignedConfigV2MediaType, mustJSON(config))
		return
	}
	c.Data(http.StatusOK, "application/json", mustJSON(config))
}

func noStore(c *gin.Context) { c.Header("Cache-Control", "no-store") }

func mustJSON(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func writeError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	code := "overlay_internal_error"
	switch {
	case errors.Is(err, ErrInvalidInput):
		status, code = http.StatusBadRequest, "invalid_request"
	case errors.Is(err, ErrInvalidToken):
		status, code = http.StatusUnauthorized, "invalid_token"
	case errors.Is(err, ErrInvalidRegistrationToken):
		status, code = http.StatusUnauthorized, "invalid_registration_token"
	case errors.Is(err, ErrRegistrationNotAvailable):
		status, code = http.StatusNotFound, "registration_not_available"
	case errors.Is(err, ErrRegistrationLimited):
		status, code = http.StatusTooManyRequests, "registration_limited"
	case errors.Is(err, ErrRegistrationRejected):
		status, code = http.StatusConflict, "registration_rejected"
	case errors.Is(err, ErrRegistrationConsumed):
		status, code = http.StatusConflict, "registration_consumed"
	case errors.Is(err, ErrRegistrationNotPending):
		status, code = http.StatusConflict, "registration_not_pending"
	case errors.Is(err, ErrRegistrationExpired):
		status, code = http.StatusGone, "registration_expired"
	case errors.Is(err, ErrInviteConstraint):
		status, code = http.StatusForbidden, "invite_constraint_mismatch"
	case errors.Is(err, ErrForbidden):
		status, code = http.StatusForbidden, "resource_access_denied"
	case errors.Is(err, ErrDeviceConflict), errors.Is(err, ErrGenerationConflict):
		status, code = http.StatusConflict, "resource_conflict"
	case errors.Is(err, ErrNotFound):
		status, code = http.StatusNotFound, "resource_not_found"
	}
	c.JSON(status, gin.H{"error": code, "message": http.StatusText(status)})
}
