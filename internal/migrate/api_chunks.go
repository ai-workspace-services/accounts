package migrate

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

const migrationChunkBytes = 2 << 20

func streamMigrationPages(ctx context.Context, tx *sql.Tx, name string, pk []string, consume func([]rawRow) error) error {
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(t)::text FROM public.`+quoteBusiness(name)+` t ORDER BY `+businessColumnList(pk))
	if err != nil {
		return errors.New("source page stream unavailable")
	}
	defer rows.Close()
	var page []rawRow
	size := 0
	for rows.Next() {
		var body string
		if rows.Scan(&body) != nil || len(body) > 1<<20 {
			return errors.New("source row exceeds 1 MiB page contract")
		}
		if len(page) > 0 && (size+len(body) > migrationChunkBytes || len(page) >= 1000) {
			if err = consume(page); err != nil {
				return err
			}
			page = nil
			size = 0
		}
		var row rawRow
		if json.Unmarshal([]byte(body), &row) != nil {
			return errors.New("source page JSON invalid")
		}
		page = append(page, row)
		size += len(body)
	}
	if rows.Err() != nil {
		return errors.New("source page stream interrupted")
	}
	if len(page) > 0 {
		return consume(page)
	}
	return nil
}

type MigrationChunk struct {
	ID      string `json:"id"`
	Table   string `json:"table"`
	Ordinal int    `json:"ordinal"`
	Rows    int    `json:"rows"`
	Bytes   int    `json:"bytes"`
	SHA256  string `json:"sha256"`
}
type migrationChunkPage struct {
	Table   string   `json:"table"`
	Ordinal int      `json:"ordinal"`
	Rows    []rawRow `json:"rows"`
}
type migrationTransferIndex struct {
	Binding  string            `json:"binding"`
	Manifest MigrationManifest `json:"manifest"`
}

func migrationChunkID(snapshot string, chunk MigrationChunk) string {
	namespace, err := uuid.Parse(snapshot)
	if err != nil {
		return ""
	}
	return uuid.NewSHA1(namespace, []byte(fmt.Sprintf("%s/%d/%s", chunk.Table, chunk.Ordinal, chunk.SHA256))).String()
}
func validateMigrationChunk(snapshot string, chunk MigrationChunk, page migrationChunkPage) error {
	body, err := json.Marshal(page)
	if err != nil || chunk.ID != migrationChunkID(snapshot, chunk) || page.Table != chunk.Table || page.Ordinal != chunk.Ordinal || len(page.Rows) != chunk.Rows || len(body) != chunk.Bytes || len(body) > migrationChunkBytes+(1<<20) || migrationDigest(page) != chunk.SHA256 {
		return errors.New("chunk integrity mismatch")
	}
	return nil
}
func validateMigrationChunkManifest(m MigrationManifest) error {
	if !migrationUUID(m.SnapshotID) || m.Format != 1 || m.Scope != MigrationAPIScope || m.SnapshotSHA256 != migrationSnapshotHash(m) || !time.Now().Before(m.ExpiresAt) {
		return errors.New("chunk manifest invalid or expired")
	}
	if !migrationSHA.MatchString(m.ToolSHA256) || !migrationSHA.MatchString(m.SourceIdentitySHA256) || !migrationSHA.MatchString(m.SourceSchemaSHA256) || !migrationSHA.MatchString(m.Authorization.SubjectSHA256) || !migrationUUID(m.Authorization.JobID) || m.Authorization.SourceAudience == "" || m.Authorization.TargetAudience == "" || m.Authorization.SourceAudience == m.Authorization.TargetAudience || !m.ExpiresAt.After(m.CreatedAt) || m.ExpiresAt.Sub(m.CreatedAt) > 24*time.Hour || m.CreatedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("chunk manifest provenance invalid")
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return err
	}
	if len(m.Tables) != len(order) || len(m.Columns) != len(order) {
		return errors.New("chunk manifest table coverage differs")
	}
	counts := map[string]int64{}
	ordinals := map[string]int{}
	seen := map[string]bool{}
	for _, name := range order {
		if m.Tables[name].Rows < 0 || !migrationSHA.MatchString(m.Tables[name].SHA256) {
			return errors.New("chunk manifest fingerprint invalid")
		}
		if migrationDigest(m.Columns[name]) != migrationDigest(tables[name].Columns) {
			return errors.New("chunk manifest field coverage differs")
		}
	}
	for _, chunk := range m.Chunks {
		if _, ok := tables[chunk.Table]; !ok {
			return errors.New("unknown chunk table")
		}
		if !migrationUUID(chunk.ID) || chunk.Ordinal != ordinals[chunk.Table] || chunk.ID != migrationChunkID(m.SnapshotID, chunk) || seen[chunk.ID] || chunk.Rows <= 0 || chunk.Rows > 1000 || chunk.Bytes <= 0 || chunk.Bytes > migrationChunkBytes+(1<<20) || !migrationSHA.MatchString(chunk.SHA256) {
			return errors.New("chunk manifest ordering or checksum invalid")
		}
		seen[chunk.ID] = true
		ordinals[chunk.Table]++
		counts[chunk.Table] += int64(chunk.Rows)
	}
	for _, name := range order {
		if counts[name] != m.Tables[name].Rows {
			return errors.New("chunk row coverage differs")
		}
	}
	return nil
}

// Publish only a committed source snapshot. Orphan pages from an interrupted
// capture remain encrypted and cannot be fetched without a completed manifest.
func (s *migrationServer) captureChunks(ctx context.Context, id, subject string) (MigrationSnapshot, error) {
	var chunks []MigrationChunk
	ordinals := map[string]int{}
	snapshot, err := captureMigrationPages(ctx, s.DB, id, s.Lifetime, s.authorization(subject), func(table string, rows []rawRow) error {
		var page []rawRow
		size := 0
		flush := func() error {
			if len(page) == 0 {
				return nil
			}
			data := migrationChunkPage{Table: table, Ordinal: ordinals[table], Rows: page}
			body, e := json.Marshal(data)
			if e != nil {
				return e
			}
			chunk := MigrationChunk{Table: table, Ordinal: data.Ordinal, Rows: len(page), Bytes: len(body), SHA256: migrationDigest(data)}
			chunk.ID = migrationChunkID(id, chunk)
			var existing migrationChunkPage
			e = s.store.read("source-chunk", chunk.ID, subject, &existing)
			if e == nil {
				e = validateMigrationChunk(id, chunk, existing)
			} else if errors.Is(e, os.ErrNotExist) {
				e = s.store.write("source-chunk", chunk.ID, subject, data)
			}
			if e != nil {
				return e
			}
			chunks = append(chunks, chunk)
			ordinals[table]++
			page = nil
			size = 0
			return nil
		}
		for _, row := range rows {
			body, e := json.Marshal(row)
			if e != nil {
				return e
			}
			if len(page) > 0 && size+len(body) > migrationChunkBytes {
				if e = flush(); e != nil {
					return e
				}
			}
			page = append(page, row)
			size += len(body)
		}
		return flush()
	})
	if err != nil {
		return MigrationSnapshot{}, err
	}
	snapshot.Rows = nil
	snapshot.Manifest.Chunks = chunks
	snapshot.Manifest.SnapshotSHA256 = migrationSnapshotHash(snapshot.Manifest)
	if err = validateMigrationChunkManifest(snapshot.Manifest); err != nil {
		return MigrationSnapshot{}, err
	}
	return snapshot, nil
}

func (s *migrationServer) downloadChunks(ctx context.Context, request MigrationPreviewRequest, subject string) (MigrationManifest, error) {
	var manifest MigrationManifest
	client, base, err := s.Peers.client(request.SourceURL)
	if err != nil {
		return manifest, err
	}
	if err = migrationHTTP(ctx, client, "GET", base+MigrationAPIBase+"/snapshots/"+request.SnapshotID+"/manifest", request.SourceToken, nil, &manifest); err != nil {
		return manifest, err
	}
	if manifest.SnapshotID != request.SnapshotID || manifest.Authorization != s.authorization(subject) {
		return manifest, errors.New("chunk manifest peer binding mismatch")
	}
	if err = validateMigrationChunkManifest(manifest); err != nil {
		return manifest, err
	}
	targetTx, err := migrationBegin(ctx, s.DB, true)
	if err != nil {
		return manifest, err
	}
	targetIdentity, identityErr := migrationIdentity(ctx, targetTx)
	_ = targetTx.Rollback()
	if identityErr != nil {
		return manifest, identityErr
	}
	if targetIdentity == manifest.SourceIdentitySHA256 {
		return manifest, errors.New("source and target are the same physical database")
	}
	binding := migrationDigest([]string{request.SourceURL, request.SnapshotID, migrationDigest(s.authorization(subject))})
	index := migrationTransferIndex{Binding: binding, Manifest: manifest}
	var old migrationTransferIndex
	err = s.store.read("transfer", request.RequestID, subject, &old)
	if err == nil {
		if old.Binding != binding || migrationDigest(old.Manifest) != migrationDigest(manifest) {
			return manifest, errors.New("transfer ID reused for different manifest")
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err = s.store.write("transfer", request.RequestID, subject, index); err != nil {
			return manifest, err
		}
	} else {
		return manifest, err
	}
	tables, order, err := migrationCatalog()
	if err != nil {
		return manifest, err
	}
	digests := map[string]*businessDigest{}
	for _, name := range order {
		digests[name] = &businessDigest{}
	}
	add := func(page migrationChunkPage) error {
		for _, row := range page.Rows {
			if err := digests[page.Table].add(row, tables[page.Table]); err != nil {
				return err
			}
		}
		return nil
	}
	for _, chunk := range manifest.Chunks {
		if err = ctx.Err(); err != nil {
			return manifest, err
		}
		var page migrationChunkPage
		err = s.store.read("target-chunk", chunk.ID, subject, &page)
		if err == nil {
			if err = validateMigrationChunk(manifest.SnapshotID, chunk, page); err != nil {
				return manifest, err
			}
			if err = add(page); err != nil {
				return manifest, err
			}
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return manifest, err
		}
		// At most two network attempts for a missing page. Valid pages are durable
		// before the next request, so a caller restart never redownloads them.
		for attempt := 0; attempt < 2; attempt++ {
			err = migrationHTTPBudget(ctx, client, "GET", base+MigrationAPIBase+"/snapshots/"+manifest.SnapshotID+"/chunks/"+chunk.ID, request.SourceToken, nil, &page, int64(chunk.Bytes)+16)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				break
			}
			var refusal *migrationHTTPStatusError
			if errors.As(err, &refusal) {
				if refusal.status != 429 && refusal.status < 500 {
					break
				}
				if attempt == 0 {
					timer := time.NewTimer(refusal.retryAfter)
					select {
					case <-ctx.Done():
						timer.Stop()
						return manifest, ctx.Err()
					case <-timer.C:
					}
				}
			}
		}
		if err != nil {
			return manifest, err
		}
		if err = validateMigrationChunk(manifest.SnapshotID, chunk, page); err != nil {
			return manifest, err
		}
		if err = s.store.write("target-chunk", chunk.ID, subject, page); err != nil {
			return manifest, err
		}
		if err = add(page); err != nil {
			return manifest, err
		}
	}
	for _, name := range order {
		actual, e := digests[name].finish()
		if e != nil || actual != manifest.Tables[name] {
			return manifest, errors.New("transferred table fingerprint mismatch")
		}
	}
	return manifest, nil
}

func (s *migrationServer) transfer(w http.ResponseWriter, r *http.Request, subject string) {
	var request MigrationPreviewRequest
	if migrationDecode(w, r, &request) != nil || !migrationUUID(request.RequestID) || !migrationUUID(request.SnapshotID) || request.SourceToken == "" || len(request.SourceToken) > 8192 {
		migrationError(w, 400, "invalid_request")
		return
	}
	var manifest MigrationManifest
	err := s.store.locked(r.Context(), func() error {
		var err error
		manifest, err = s.downloadChunks(r.Context(), request, subject)
		return err
	})
	if err != nil {
		migrationError(w, 409, "chunk_transfer_incomplete_retry_same_ids")
		return
	}
	receipt := MigrationPlan{PlanID: request.RequestID, State: "transfer_staged", Snapshot: manifest, ToolSHA256: migrationExecutableSHA256(), CreatedAt: manifest.CreatedAt, ExpiresAt: manifest.ExpiresAt}
	receipt.PlanSHA256 = migrationPlanHash(receipt)
	migrationJSON(w, 200, receipt)
}
