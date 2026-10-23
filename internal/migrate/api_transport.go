package migrate

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const MigrationAPIBase = "/api/migrations/v1"
const migrationTokenUse = "accounts-migration-v1"

func migrationUUID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

type MigrationClaims struct {
	Use            string   `json:"token_use"`
	Scopes         []string `json:"scopes"`
	SourceAudience string   `json:"source_audience"`
	TargetAudience string   `json:"target_audience"`
	JobID          string   `json:"job_id"`
	jwt.RegisteredClaims
}

// A separate key and audience, never Accounts JWT/service-token middleware.
func MintMigrationToken(key []byte, audience, subject string, scopes []string, lifetime time.Duration, binding MigrationAuthorization) (string, error) {
	if len(key) != 32 || audience == "" || subject == "" || len(subject) > 128 || lifetime <= 0 || lifetime > 15*time.Minute || len(scopes) == 0 {
		return "", errors.New("invalid migration token parameters")
	}
	if binding.SourceAudience == "" || binding.TargetAudience == "" || binding.SourceAudience == binding.TargetAudience || !migrationUUID(binding.JobID) {
		return "", errors.New("explicit source/target audience and migration job UUID required")
	}
	for _, scope := range scopes {
		if scope != "source.read" && scope != "target.plan" && scope != "target.apply" {
			return "", errors.New("unknown migration scope")
		}
		if (scope == "source.read" && audience != binding.SourceAudience) || (scope != "source.read" && audience != binding.TargetAudience) {
			return "", errors.New("scope does not match bound migration endpoint")
		}
	}
	now := time.Now().UTC()
	claims := MigrationClaims{Use: migrationTokenUse, Scopes: scopes, SourceAudience: binding.SourceAudience, TargetAudience: binding.TargetAudience, JobID: binding.JobID, RegisteredClaims: jwt.RegisteredClaims{Issuer: migrationTokenUse, Subject: subject, Audience: jwt.ClaimStrings{audience}, IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(lifetime)), ID: uuid.NewString()}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(key)
}

type migrationStore struct {
	dir  string
	aead cipher.AEAD
}

func newMigrationStore(dir string, key []byte) (*migrationStore, error) {
	if !filepath.IsAbs(dir) || len(key) != 32 {
		return nil, errors.New("absolute staging directory and 32-byte staging key required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("staging directory unavailable")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return nil, errors.New("staging directory must be private mode 0700 without symlinks")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &migrationStore{dir: dir, aead: aead}, nil
}

func (s *migrationStore) locked(ctx context.Context, fn func() error) error {
	f, err := os.OpenFile(filepath.Join(s.dir, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("staging lock unavailable")
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			return errors.New("staging lock failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *migrationStore) path(kind, id string) string {
	return filepath.Join(s.dir, kind+"-"+id+".gcm")
}
func (s *migrationStore) read(kind, id, subject string, out any) error {
	if !migrationUUID(id) {
		return errors.New("invalid object ID")
	}
	f, err := os.OpenFile(s.path(kind, id), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > MigrationAPIMaxBytes+1024 {
		return errors.New("invalid staging object")
	}
	b, err := io.ReadAll(io.LimitReader(f, MigrationAPIMaxBytes+1025))
	if err != nil || len(b) < s.aead.NonceSize() {
		return errors.New("invalid encrypted staging object")
	}
	plain, err := s.aead.Open(nil, b[:s.aead.NonceSize()], b[s.aead.NonceSize():], []byte(kind+"/"+id+"/"+subject))
	if err != nil {
		return errors.New("staging authentication failed")
	}
	if json.Unmarshal(plain, out) != nil {
		return errors.New("invalid staging payload")
	}
	return nil
}
func (s *migrationStore) write(kind, id, subject string, value any) error {
	if !migrationUUID(id) {
		return errors.New("invalid object ID")
	}
	b, err := json.Marshal(value)
	if err != nil || len(b) > MigrationAPIMaxBytes {
		return errors.New("staging payload exceeds budget")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, b, []byte(kind+"/"+id+"/"+subject))
	f, err := os.CreateTemp(s.dir, ".pending-")
	if err != nil {
		return errors.New("staging persistence failed")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(sealed); err == nil {
		err = f.Sync()
	}
	if err != nil {
		return errors.New("staging persistence failed")
	}
	if err = f.Close(); err != nil {
		return err
	}
	// Hard link is atomic and cannot overwrite another immutable object.
	if err = os.Link(f.Name(), s.path(kind, id)); err != nil {
		return errors.New("staging ID already exists or persistence failed")
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Exact URL plus explicit destination networks, resolution pinned for every
// connection. No proxy env, redirects, implicit metadata/private network access.
type MigrationPeerPolicy struct {
	URLs              []string
	Networks          []netip.Prefix
	AllowLoopbackHTTP bool
}

func migrationURL(value string, allowLocal bool) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("migration URL must be an origin without credentials, query or path")
	}
	if u.Scheme != "https" {
		ip, e := netip.ParseAddr(u.Hostname())
		if u.Scheme != "http" || !allowLocal || (u.Hostname() != "localhost" && (e != nil || !ip.IsLoopback())) {
			return nil, errors.New("migration endpoint requires HTTPS")
		}
	}
	u.Path = ""
	return u, nil
}

func (p MigrationPeerPolicy) client(value string) (*http.Client, string, error) {
	u, err := migrationURL(value, p.AllowLoopbackHTTP)
	if err != nil {
		return nil, "", err
	}
	allowed := false
	for _, v := range p.URLs {
		other, e := migrationURL(v, p.AllowLoopbackHTTP)
		if e == nil && other.String() == u.String() {
			allowed = true
		}
	}
	if !allowed || len(p.Networks) == 0 {
		return nil, "", errors.New("source peer URL or network is not allowlisted")
	}
	transport := &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, DisableKeepAlives: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, e := net.SplitHostPort(address)
		if e != nil || host != u.Hostname() {
			return nil, errors.New("peer host changed")
		}
		addresses, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if e != nil || len(addresses) == 0 {
			return nil, errors.New("peer DNS unavailable")
		}
		for _, ip := range addresses {
			ip = ip.Unmap()
			if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
				return nil, errors.New("unsafe peer address")
			}
			ok := false
			for _, prefix := range p.Networks {
				if prefix.Contains(ip) {
					ok = true
				}
			}
			if !ok {
				return nil, errors.New("peer address outside allowlist")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].String(), port))
	}
	return &http.Client{Transport: transport, Timeout: 3 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, u.String(), nil
}

type MigrationServerOptions struct {
	DB                 *sql.DB
	Role               string
	Audience           string
	AuthKey            []byte
	StagingKey         []byte
	StagingDirectory   string
	Lifetime           time.Duration
	Peers              MigrationPeerPolicy
	BackupGate         MigrationBackupGate
	InitializeReceipts bool
	ReceiptOwnerRole   string
	SourceAudience     string
	TargetAudience     string
	JobID              string
}
type migrationServer struct {
	MigrationServerOptions
	store  *migrationStore
	active chan struct{}
}
type migrationStagePlan struct {
	Binding  string            `json:"binding"`
	Plan     MigrationPlan     `json:"plan"`
	Snapshot MigrationSnapshot `json:"snapshot"`
}
type MigrationPreviewRequest struct {
	RequestID   string `json:"request_id"`
	SourceURL   string `json:"source_url"`
	SnapshotID  string `json:"snapshot_id"`
	SourceToken string `json:"source_token"`
}
type migrationStreamRecord struct {
	Manifest *MigrationManifest `json:"manifest,omitempty"`
	Table    string             `json:"table,omitempty"`
	Row      rawRow             `json:"row,omitempty"`
}

func NewMigrationHandler(options MigrationServerOptions) (http.Handler, error) {
	if options.SourceAudience == "" || options.TargetAudience == "" || options.SourceAudience == options.TargetAudience || !migrationUUID(options.JobID) || (options.Role == "source" && options.Audience != options.SourceAudience) || (options.Role == "target" && options.Audience != options.TargetAudience) {
		return nil, errors.New("listener must bind an explicit source/target audience pair and job UUID")
	}
	if options.DB == nil || (options.Role != "source" && options.Role != "target") || options.Audience == "" || len(options.AuthKey) != 32 || bytes.Equal(options.AuthKey, options.StagingKey) || options.Lifetime <= 0 || options.Lifetime > 24*time.Hour {
		return nil, errors.New("invalid migration listener configuration")
	}
	store, err := newMigrationStore(options.StagingDirectory, options.StagingKey)
	if err != nil {
		return nil, err
	}
	if options.InitializeReceipts {
		if options.Role != "target" || options.BackupGate == nil || options.ReceiptOwnerRole == "" {
			return nil, errors.New("receipt initialization requires configured apply gate and explicit migration owner role")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var role string
		if options.DB.QueryRowContext(ctx, `SELECT current_user`).Scan(&role) != nil || role != options.ReceiptOwnerRole {
			return nil, errors.New("receipt initialization connection does not match migration owner role")
		}
		if err = InitializeMigrationReceipts(ctx, options.DB); err != nil {
			return nil, err
		}
	}
	s := &migrationServer{MigrationServerOptions: options, store: store, active: make(chan struct{}, 1)}
	return http.HandlerFunc(s.serve), nil
}

func (s *migrationServer) authenticate(r *http.Request, scope string) (string, int) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") || len(value) > 8192 {
		return "", 401
	}
	claims := &MigrationClaims{}
	_, err := jwt.ParseWithClaims(strings.TrimPrefix(value, "Bearer "), claims, func(token *jwt.Token) (any, error) { return s.AuthKey, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(s.Audience), jwt.WithIssuer(migrationTokenUse), jwt.WithExpirationRequired(), jwt.WithIssuedAt())
	if err != nil || claims.Use != migrationTokenUse || claims.Subject == "" || len(claims.Subject) > 128 || claims.ID == "" || claims.IssuedAt == nil || claims.NotBefore == nil || claims.ExpiresAt == nil || claims.ExpiresAt.Sub(claims.IssuedAt.Time) > 15*time.Minute || !claims.ExpiresAt.After(claims.IssuedAt.Time) {
		return "", 401
	}
	if claims.SourceAudience != s.SourceAudience || claims.TargetAudience != s.TargetAudience || claims.JobID != s.JobID {
		return "", 403
	}
	for _, v := range claims.Scopes {
		if v == scope {
			return claims.Subject, 0
		}
	}
	return "", 403
}

func migrationJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func migrationError(w http.ResponseWriter, status int, code string) {
	migrationJSON(w, status, map[string]string{"error": code})
}
func migrationDecode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("trailing request data")
	}
	return nil
}

func (s *migrationServer) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := strings.TrimPrefix(r.URL.Path, MigrationAPIBase)
	if path == r.URL.Path {
		migrationError(w, 404, "unsupported_endpoint")
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	scope := ""
	method := ""
	if s.Role == "source" && path == "/snapshots" {
		scope = "source"
		method = "POST"
	} else if s.Role == "source" && len(parts) == 3 && parts[0] == "snapshots" && migrationUUID(parts[1]) && (parts[2] == "manifest" || parts[2] == "stream") {
		scope = "source"
		method = "GET"
	} else if s.Role == "target" && path == "/plans/preview" {
		scope = "preview"
		method = "POST"
	} else if s.Role == "target" && len(parts) == 3 && parts[0] == "plans" && migrationUUID(parts[1]) && parts[2] == "apply" {
		scope = "apply"
		method = "POST"
	} else if s.Role == "target" && len(parts) == 2 && parts[0] == "plans" && migrationUUID(parts[1]) {
		scope = "status"
		method = "GET"
	}
	if scope == "" {
		migrationError(w, 404, "unsupported_endpoint")
		return
	}
	if r.Method != method {
		w.Header().Set("Allow", method)
		migrationError(w, 405, "unsupported_method")
		return
	}
	claimScope := "target.plan"
	if scope == "source" {
		claimScope = "source.read"
	} else if scope == "apply" {
		claimScope = "target.apply"
	}
	subject, status := s.authenticate(r, claimScope)
	if status != 0 {
		migrationError(w, status, "migration_auth_denied")
		return
	}
	select {
	case s.active <- struct{}{}:
		defer func() { <-s.active }()
	default:
		migrationError(w, 503, "migration_listener_busy_retry_same_id")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	if scope == "source" {
		s.source(w, r, parts, subject)
		return
	}
	if scope == "preview" {
		s.preview(w, r, subject)
		return
	}
	s.target(w, r, parts, subject, scope)
}

func (s *migrationServer) source(w http.ResponseWriter, r *http.Request, parts []string, subject string) {
	var snapshot MigrationSnapshot
	id := ""
	if r.Method == "POST" {
		var request struct {
			RequestID string `json:"request_id"`
		}
		if migrationDecode(w, r, &request) != nil || !migrationUUID(request.RequestID) {
			migrationError(w, 400, "invalid_request")
			return
		}
		id = request.RequestID
	} else {
		id = parts[1]
	}
	err := s.store.locked(r.Context(), func() error {
		err := s.store.read("snapshot", id, subject, &snapshot)
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) || r.Method != "POST" {
			return err
		}
		snapshot, err = CaptureMigrationSnapshot(r.Context(), s.DB, id, s.Lifetime, s.authorization(subject))
		if err != nil {
			return err
		}
		return s.store.write("snapshot", id, subject, snapshot)
	})
	if err != nil {
		migrationError(w, 409, "snapshot_unavailable_or_conflicted")
		return
	}
	if snapshot.Manifest.Authorization != s.authorization(subject) {
		migrationError(w, 403, "snapshot_peer_binding_mismatch")
		return
	}
	if !time.Now().Before(snapshot.Manifest.ExpiresAt) {
		migrationError(w, 410, "snapshot_expired")
		return
	}
	if r.Method == "POST" || parts[2] == "manifest" {
		migrationJSON(w, 200, snapshot.Manifest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	if encoder.Encode(migrationStreamRecord{Manifest: &snapshot.Manifest}) != nil {
		return
	}
	_, order, err := migrationCatalog()
	if err != nil {
		return
	}
	for _, name := range order {
		for _, row := range snapshot.Rows[name] {
			if encoder.Encode(migrationStreamRecord{Table: name, Row: row}) != nil {
				return
			}
		}
	}
}

func migrationHTTP(ctx context.Context, client *http.Client, method, endpoint, token string, input, output any) error {
	var body io.Reader
	if input != nil {
		b, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return errors.New("invalid migration request")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if input != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(r)
	if err != nil {
		return errors.New("migration endpoint unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode == 404 || response.StatusCode == 405 || response.StatusCode == 501 {
		return errors.New("unsupported migration endpoint; no fallback")
	}
	if response.StatusCode != 200 {
		return fmt.Errorf("migration endpoint refused request (HTTP %d)", response.StatusCode)
	}
	if output == nil {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(response.Body, MigrationAPIMaxBytes+1))
	if err != nil || len(b) > MigrationAPIMaxBytes {
		return errors.New("migration response exceeds budget")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if decoder.Decode(output) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid migration response")
	}
	return nil
}

func (s *migrationServer) authorization(subject string) MigrationAuthorization {
	return MigrationAuthorization{SourceAudience: s.SourceAudience, TargetAudience: s.TargetAudience, JobID: s.JobID, SubjectSHA256: migrationDigest(subject)}
}

func (s *migrationServer) pull(ctx context.Context, request MigrationPreviewRequest, subject string) (MigrationSnapshot, error) {
	var snapshot MigrationSnapshot
	client, base, err := s.Peers.client(request.SourceURL)
	if err != nil {
		return snapshot, err
	}
	var manifest MigrationManifest
	if err = migrationHTTP(ctx, client, "GET", base+MigrationAPIBase+"/snapshots/"+request.SnapshotID+"/manifest", request.SourceToken, nil, &manifest); err != nil {
		return snapshot, err
	}
	if manifest.Authorization != s.authorization(subject) {
		return snapshot, errors.New("source token belongs to a different migration peer pair, job or operator")
	}
	r, err := http.NewRequestWithContext(ctx, "GET", base+MigrationAPIBase+"/snapshots/"+request.SnapshotID+"/stream", nil)
	if err != nil {
		return snapshot, err
	}
	r.Header.Set("Authorization", "Bearer "+request.SourceToken)
	response, err := client.Do(r)
	if err != nil {
		return snapshot, errors.New("snapshot stream unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return snapshot, errors.New("snapshot stream refused")
	}
	limited := &io.LimitedReader{R: response.Body, N: MigrationAPIMaxBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var first migrationStreamRecord
	if decoder.Decode(&first) != nil || first.Manifest == nil || first.Table != "" || first.Row != nil || migrationDigest(*first.Manifest) != migrationDigest(manifest) || manifest.SnapshotID != request.SnapshotID {
		return snapshot, errors.New("snapshot manifest changed")
	}
	snapshot.Manifest = manifest
	snapshot.Rows = map[string][]rawRow{}
	tables, order, err := migrationCatalog()
	if err != nil {
		return snapshot, err
	}
	for _, name := range order {
		snapshot.Rows[name] = []rawRow{}
	}
	for {
		var record migrationStreamRecord
		err = decoder.Decode(&record)
		if err == io.EOF {
			break
		}
		if err != nil || record.Manifest != nil || record.Row == nil {
			return snapshot, errors.New("snapshot stream corrupt")
		}
		if _, ok := tables[record.Table]; !ok {
			return snapshot, errors.New("snapshot stream scope mismatch")
		}
		snapshot.Rows[record.Table] = append(snapshot.Rows[record.Table], record.Row)
	}
	if limited.N <= 0 {
		return snapshot, errors.New("snapshot stream exceeds budget")
	}
	if err = validateMigrationSnapshot(snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (s *migrationServer) preview(w http.ResponseWriter, r *http.Request, subject string) {
	var request MigrationPreviewRequest
	if migrationDecode(w, r, &request) != nil || !migrationUUID(request.RequestID) || !migrationUUID(request.SnapshotID) || request.SourceToken == "" || len(request.SourceToken) > 8192 {
		migrationError(w, 400, "invalid_request")
		return
	}
	if _, _, err := s.Peers.client(request.SourceURL); err != nil {
		migrationError(w, 403, "source_peer_denied")
		return
	}
	binding := migrationDigest([]string{request.SourceURL, request.SnapshotID, migrationDigest(s.authorization(subject))})
	var stage migrationStagePlan
	err := s.store.locked(r.Context(), func() error {
		err := s.store.read("plan", request.RequestID, subject, &stage)
		if err == nil {
			if stage.Binding != binding {
				return errors.New("plan request ID reused")
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		snapshot, err := s.pull(r.Context(), request, subject)
		if err != nil {
			return err
		}
		plan, err := PreviewMigration(r.Context(), s.DB, snapshot, request.RequestID, s.Lifetime)
		if err != nil {
			return err
		}
		stage = migrationStagePlan{Binding: binding, Plan: plan, Snapshot: snapshot}
		return s.store.write("plan", request.RequestID, subject, stage)
	})
	if err != nil {
		migrationError(w, 409, "preview_conflicted_or_unavailable")
		return
	}
	if stage.Plan.Snapshot.Authorization != s.authorization(subject) {
		migrationError(w, 403, "plan_peer_binding_mismatch")
		return
	}
	if !time.Now().Before(stage.Plan.ExpiresAt) {
		migrationError(w, 410, "plan_expired")
		return
	}
	migrationJSON(w, 200, stage.Plan)
}

func (s *migrationServer) target(w http.ResponseWriter, r *http.Request, parts []string, subject, scope string) {
	var stage migrationStagePlan
	if err := s.store.read("plan", parts[1], subject, &stage); err != nil {
		migrationError(w, 409, "plan_unavailable")
		return
	}
	if stage.Plan.Snapshot.Authorization != s.authorization(subject) {
		migrationError(w, 403, "plan_peer_binding_mismatch")
		return
	}
	if scope == "status" {
		done, ok, err := ReadMigrationReceipt(r.Context(), s.DB, stage.Plan.PlanID, stage.Plan.PlanSHA256, subject)
		if err != nil {
			migrationError(w, 409, "receipt_unavailable")
			return
		}
		if ok {
			migrationJSON(w, 200, done)
		} else if !time.Now().Before(stage.Plan.ExpiresAt) {
			migrationError(w, 410, "plan_expired")
		} else {
			migrationJSON(w, 200, stage.Plan)
		}
		return
	}
	var request struct {
		PlanSHA256 string `json:"plan_sha256"`
	}
	if migrationDecode(w, r, &request) != nil {
		migrationError(w, 400, "invalid_request")
		return
	}
	if request.PlanSHA256 != stage.Plan.PlanSHA256 {
		migrationError(w, 409, "reviewed_plan_hash_mismatch")
		return
	}
	// A committed retry only reads its transactional receipt, even if the backup
	// proof has subsequently expired. New writes require a fresh owner gate.
	if done, ok, err := ReadMigrationReceipt(r.Context(), s.DB, stage.Plan.PlanID, request.PlanSHA256, subject); err != nil {
		migrationError(w, 409, "receipt_unavailable")
		return
	} else if ok {
		migrationJSON(w, 200, done)
		return
	}
	if s.BackupGate == nil {
		migrationError(w, 403, "apply_requires_migration_owner_backup_gate")
		return
	}
	if err := s.BackupGate(r.Context(), stage.Plan); err != nil {
		migrationError(w, 409, "verified_target_backup_required")
		return
	}
	plan, err := ApplyMigration(r.Context(), s.DB, stage.Snapshot, stage.Plan, request.PlanSHA256, subject, s.BackupGate)
	if err != nil {
		migrationError(w, 409, "apply_conflicted_or_unavailable")
		return
	}
	migrationJSON(w, 200, plan)
}

type MigrationClientOptions struct {
	SourceURL, TargetURL, SourceToken, TargetToken, SnapshotID, PlanID, PlanSHA256 string
	Apply, Status, AllowLoopbackHTTP                                               bool
}

func validateRemoteMigrationPlan(plan MigrationPlan, id, hash string) error {
	if plan.PlanID != id || (hash != "" && plan.PlanSHA256 != hash) {
		return errors.New("migration response plan binding mismatch")
	}
	preview := plan
	if plan.State == "applied" {
		preview.State = "preview"
	} else if plan.State != "preview" && plan.State != "conflict" {
		return errors.New("unknown migration plan state")
	}
	if plan.PlanSHA256 != migrationPlanHash(preview) {
		return errors.New("migration response plan hash mismatch")
	}
	return nil
}

// Credentials are supplied in memory by the CLI's env/Vault references.
func RunMigrationClient(ctx context.Context, options MigrationClientOptions) (MigrationPlan, error) {
	var plan MigrationPlan
	target, err := migrationURL(options.TargetURL, options.AllowLoopbackHTTP)
	if err != nil {
		return plan, err
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 3 * time.Minute}, Timeout: 5 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	if options.TargetToken == "" {
		return plan, errors.New("target migration credential required")
	}
	if options.Apply || options.Status {
		if !migrationUUID(options.PlanID) || (options.Apply && options.PlanSHA256 == "") {
			return plan, errors.New("exact plan ID and reviewed hash required")
		}
		method := "GET"
		endpoint := target.String() + MigrationAPIBase + "/plans/" + options.PlanID
		var body any
		if options.Apply {
			method = "POST"
			endpoint += "/apply"
			body = map[string]string{"plan_sha256": options.PlanSHA256}
		}
		err = migrationHTTP(ctx, client, method, endpoint, options.TargetToken, body, &plan)
		if err == nil {
			err = validateRemoteMigrationPlan(plan, options.PlanID, options.PlanSHA256)
		}
		return plan, err
	}
	source, err := migrationURL(options.SourceURL, options.AllowLoopbackHTTP)
	if err != nil {
		return plan, err
	}
	if source.String() == target.String() {
		return plan, errors.New("source and target URLs must differ")
	}
	if options.SourceToken == "" {
		return plan, errors.New("source migration credential required")
	}
	if options.SnapshotID == "" {
		options.SnapshotID = uuid.NewString()
	}
	if options.PlanID == "" {
		options.PlanID = uuid.NewString()
	}
	if !migrationUUID(options.SnapshotID) || !migrationUUID(options.PlanID) {
		return plan, errors.New("invalid request IDs")
	}
	var manifest MigrationManifest
	if err = migrationHTTP(ctx, client, "POST", source.String()+MigrationAPIBase+"/snapshots", options.SourceToken, map[string]string{"request_id": options.SnapshotID}, &manifest); err != nil {
		return plan, err
	}
	if manifest.SnapshotID != options.SnapshotID || manifest.SnapshotSHA256 != migrationSnapshotHash(manifest) {
		return plan, errors.New("source manifest binding mismatch")
	}
	request := MigrationPreviewRequest{RequestID: options.PlanID, SourceURL: source.String(), SnapshotID: options.SnapshotID, SourceToken: options.SourceToken}
	err = migrationHTTP(ctx, client, "POST", target.String()+MigrationAPIBase+"/plans/preview", options.TargetToken, request, &plan)
	if err == nil && plan.Snapshot.SnapshotSHA256 != manifest.SnapshotSHA256 {
		return MigrationPlan{}, errors.New("target preview snapshot differs")
	}
	if err == nil {
		err = validateRemoteMigrationPlan(plan, options.PlanID, "")
	}
	return plan, err
}
