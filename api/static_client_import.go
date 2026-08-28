package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"github.com/gin-gonic/gin"

	"account/internal/store"
)

const (
	staticImportMediaType    = "application/vnd.xconnect.static-client-import.v1+json"
	staticImportKind         = "xconnect.accounts.static-client-import"
	staticImportVariable     = "xworkmate_bridge_distributed_vpn_clients"
	staticImportMigrationTag = "migration:static-group-vars"
	maxStaticImportBody      = 4 << 20
)

var (
	staticImportIDPattern          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:-]{2,127}$`)
	staticImportUUIDPattern        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	staticImportDigestPattern      = regexp.MustCompile(`^[a-f0-9]{64}$`)
	staticImportIdempotencyPattern = regexp.MustCompile(`^sha256-[a-f0-9]{64}$`)
)

type staticImportDocument struct {
	SchemaVersion int                  `json:"schema_version"`
	Kind          string               `json:"kind"`
	NetworkID     string               `json:"network_id"`
	OwnerUserID   string               `json:"owner_user_id"`
	Source        staticImportSource   `json:"source"`
	Devices       []staticImportDevice `json:"devices"`
}
type staticImportSource struct {
	Kind           string `json:"kind"`
	Variable       string `json:"variable"`
	BaselineSHA256 string `json:"baseline_sha256"`
}
type staticImportDevice struct {
	DeviceID           string   `json:"device_id"`
	WireGuardPublicKey string   `json:"wireguard_public_key"`
	Addresses          []string `json:"addresses"`
	Tags               []string `json:"tags"`
	Attachments        []string `json:"attachments"`
}

func sensitiveStaticImportTag(value string) bool {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"private_key:", "preshared_key:", "auth_id:", "password:", "token:", "secret:", "credential:", "uuid:", "vless_uuid:"} {
		if strings.HasPrefix(normalized, prefix) {
			return true
		}
	}
	return false
}

func normalizeImportStrings(values []string, validate func(string) bool) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] || !validate(value) {
			return nil, errors.New("invalid or duplicated string")
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func normalizeStaticImport(document staticImportDocument) (staticImportDocument, error) {
	if document.SchemaVersion != 1 || document.Kind != staticImportKind || !staticImportIDPattern.MatchString(document.NetworkID) || !staticImportUUIDPattern.MatchString(document.OwnerUserID) || document.Source.Kind != "ansible-group-vars" || document.Source.Variable != staticImportVariable || !staticImportDigestPattern.MatchString(document.Source.BaselineSHA256) {
		return staticImportDocument{}, errors.New("static import contract is invalid")
	}
	if len(document.Devices) == 0 || len(document.Devices) > 10000 {
		return staticImportDocument{}, errors.New("static import devices must contain between 1 and 10000 entries")
	}
	seenIDs, seenKeys, seenAddresses := map[string]bool{}, map[string]bool{}, map[string]bool{}
	normalized := make([]staticImportDevice, 0, len(document.Devices))
	for _, device := range document.Devices {
		if !staticImportIDPattern.MatchString(device.DeviceID) || seenIDs[device.DeviceID] {
			return staticImportDocument{}, errors.New("static import device identity is invalid or duplicated")
		}
		key, err := base64.StdEncoding.DecodeString(device.WireGuardPublicKey)
		if err != nil || len(key) != 32 || seenKeys[device.WireGuardPublicKey] {
			return staticImportDocument{}, errors.New("static import WireGuard public key is invalid or duplicated")
		}
		if len(device.Addresses) != 1 {
			return staticImportDocument{}, errors.New("static import device requires one IPv4 /32")
		}
		prefix, err := netip.ParsePrefix(device.Addresses[0])
		if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 32 || prefix.String() != device.Addresses[0] || seenAddresses[device.Addresses[0]] {
			return staticImportDocument{}, errors.New("static import address is invalid or duplicated")
		}
		if len(device.Attachments) == 0 {
			return staticImportDocument{}, errors.New("static import attachments are required")
		}
		attachments, err := normalizeImportStrings(device.Attachments, staticImportIDPattern.MatchString)
		if err != nil {
			return staticImportDocument{}, errors.New("static import attachments are invalid")
		}
		tags, err := normalizeImportStrings(device.Tags, func(value string) bool {
			return strings.TrimSpace(value) == value && value != "" && len(value) <= 128 && !sensitiveStaticImportTag(value)
		})
		if err != nil {
			return staticImportDocument{}, errors.New("static import tags are invalid")
		}
		foundMigration := false
		for _, tag := range tags {
			if tag == staticImportMigrationTag {
				foundMigration = true
			}
		}
		if !foundMigration {
			return staticImportDocument{}, errors.New("static import migration source tag is required")
		}
		seenIDs[device.DeviceID], seenKeys[device.WireGuardPublicKey], seenAddresses[device.Addresses[0]] = true, true, true
		normalized = append(normalized, staticImportDevice{DeviceID: device.DeviceID, WireGuardPublicKey: device.WireGuardPublicKey, Addresses: []string{prefix.String()}, Tags: tags, Attachments: attachments})
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].DeviceID < normalized[j].DeviceID })
	baselineRaw, _ := json.Marshal(normalized)
	baseline := sha256.Sum256(baselineRaw)
	if document.Source.BaselineSHA256 != hex.EncodeToString(baseline[:]) {
		return staticImportDocument{}, errors.New("static import baseline digest mismatch")
	}
	document.Devices = normalized
	return document, nil
}

func decodeCanonicalStaticImport(c *gin.Context) (staticImportDocument, []byte, error) {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != staticImportMediaType {
		return staticImportDocument{}, nil, errors.New("unsupported media type")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxStaticImportBody))
	if err != nil {
		return staticImportDocument{}, nil, errors.New("static import body is too large")
	}
	var document staticImportDocument
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return staticImportDocument{}, nil, errors.New("static import body is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return staticImportDocument{}, nil, errors.New("static import must contain one document")
	}
	document, err = normalizeStaticImport(document)
	if err != nil {
		return staticImportDocument{}, nil, err
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return staticImportDocument{}, nil, err
	}
	if !bytes.Equal(raw, canonical) {
		return staticImportDocument{}, nil, errors.New("static import body is not canonical v1 JSON")
	}
	return document, canonical, nil
}

func (h *handler) importOverlayStaticClients(c *gin.Context) {
	document, canonical, err := decodeCanonicalStaticImport(c)
	if err != nil {
		if strings.Contains(err.Error(), "media type") {
			respondError(c, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		} else {
			respondError(c, http.StatusBadRequest, "invalid_static_import", err.Error())
		}
		return
	}
	bodyDigest := sha256.Sum256(canonical)
	bodyHex := hex.EncodeToString(bodyDigest[:])
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if !staticImportIdempotencyPattern.MatchString(idempotencyKey) || idempotencyKey != "sha256-"+bodyHex {
		respondError(c, http.StatusConflict, "idempotency_key_mismatch", "Idempotency-Key must match canonical request body")
		return
	}
	input := &store.OverlayStaticImport{IdempotencyKey: idempotencyKey, BodySHA256: bodyHex, OwnerUserID: document.OwnerUserID, NetworkID: document.NetworkID, SourceKind: document.Source.Kind, SourceVariable: document.Source.Variable, BaselineSHA256: document.Source.BaselineSHA256, Devices: make([]store.OverlayProjectionDevice, 0, len(document.Devices))}
	for _, device := range document.Devices {
		input.Devices = append(input.Devices, store.OverlayProjectionDevice{Device: store.OverlayDevice{ID: device.DeviceID, UserID: document.OwnerUserID, NetworkID: document.NetworkID, Name: device.DeviceID, Platform: "legacy-import", WireGuardPublicKey: device.WireGuardPublicKey, WireGuardAddress: device.Addresses[0]}, Tags: device.Tags, Attachments: device.Attachments})
	}
	audit := &store.AuditLog{Action: store.AuditActionOverlayStaticImport, Details: map[string]any{"target_uuid": document.OwnerUserID, "network_id": document.NetworkID, "baseline_sha256": document.Source.BaselineSHA256, "device_count": len(document.Devices), "idempotency_key": idempotencyKey}}
	receipt, _, err := h.store.ImportOverlayStaticClients(c.Request.Context(), input, audit)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			respondError(c, http.StatusNotFound, "owner_user_not_found", "import owner user does not exist")
		case errors.Is(err, store.ErrOverlayStaticImportConflict):
			respondError(c, http.StatusConflict, "static_device_conflict", "device conflicts with an existing owner, network, or key")
		case errors.Is(err, store.ErrOverlayStaticImportIdempotency):
			respondError(c, http.StatusConflict, "idempotency_key_reused", "Idempotency-Key is bound to another body")
		default:
			respondError(c, http.StatusServiceUnavailable, "static_import_failed", "failed to persist static client import")
		}
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, receipt)
}
