package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	schema "account/sql"
	"github.com/google/uuid"
)

var generationNamePattern = regexp.MustCompile(`^([a-z][a-z0-9-]*)-v([1-9][0-9]*)$`)

// GenerationUserOptions binds a post-cutover identity operation to adjacent
// immutable generations and the reviewed native target schema.
type GenerationUserOptions struct {
	SourceGeneration      string
	TargetGeneration      string
	Environment           string
	SchemaSHA256          string
	BillingSHA256         string
	ExpectedTargetVersion uint
}

type GenerationUserDiff struct {
	SourceOnlyUsers        int  `json:"source_only_users"`
	TargetOnlyUsers        int  `json:"target_only_users"`
	UserIDMismatches       int  `json:"user_id_mismatches"`
	EmailMismatches        int  `json:"email_mismatches"`
	ProxyUUIDMismatches    int  `json:"proxy_uuid_mismatches"`
	PasswordHashMismatches int  `json:"password_hash_mismatches"`
	Equal                  bool `json:"equal"`
}

// GenerationUserBaseline is a private, non-PII user-ID set captured after the
// final source sync and before DNS handoff. User IDs are one-way hashed so the
// evidence can classify later source arrivals without storing account rows.
type GenerationUserBaseline struct {
	Format              int                `json:"format"`
	Algorithm           string             `json:"algorithm"`
	SourceGeneration    string             `json:"source_generation"`
	TargetGeneration    string             `json:"target_generation"`
	CapturedAt          time.Time          `json:"captured_at"`
	SourceUserCount     int                `json:"source_user_count"`
	TargetUserCount     int                `json:"target_user_count"`
	SourceUserIDHashes  []string           `json:"source_user_id_hashes"`
	SourceUserSetSHA256 string             `json:"source_user_set_sha256"`
	IdentityDiff        GenerationUserDiff `json:"identity_diff"`
	BaselineSHA256      string             `json:"baseline_sha256"`
}

type LateUsersAudit struct {
	Format                        int                `json:"format"`
	SourceGeneration              string             `json:"source_generation"`
	TargetGeneration              string             `json:"target_generation"`
	BaselineCapturedAt            time.Time          `json:"baseline_captured_at"`
	ObservedAt                    time.Time          `json:"observed_at"`
	BaselineSourceUserCount       int                `json:"baseline_source_user_count"`
	CurrentSourceUserCount        int                `json:"current_source_user_count"`
	CurrentTargetUserCount        int                `json:"current_target_user_count"`
	SourceUsersAbsentFromBaseline int                `json:"source_users_absent_from_baseline"`
	LateSourceUsersMissingTarget  int                `json:"late_source_users_missing_target"`
	IdentityDiff                  GenerationUserDiff `json:"identity_diff"`
	SourceWriteIsolationAnomaly   bool               `json:"source_write_isolation_anomaly"`
	Result                        string             `json:"result"`
}

type SingleGenerationUserOptions struct {
	GenerationUserOptions
	SourceUserID        string
	Apply               bool
	ConfirmSourceUserID string
	SourceWritersPaused bool
	ExpectedPlanSHA256  string
}

type SingleGenerationUserReceipt struct {
	Format                  int       `json:"format"`
	ReceiptID               string    `json:"receipt_id"`
	SourceGeneration        string    `json:"source_generation"`
	TargetGeneration        string    `json:"target_generation"`
	SourceUserIDSHA256      string    `json:"source_user_id_sha256"`
	ObservedAt              time.Time `json:"observed_at"`
	Result                  string    `json:"result"`
	PlanSHA256              string    `json:"plan_sha256"`
	SourceReadOnly          bool      `json:"source_read_only"`
	TargetWrites            bool      `json:"target_writes"`
	UserRowsInserted        int       `json:"user_rows_inserted"`
	OAuthIdentityRowsCopied int       `json:"oauth_identity_rows_copied"`
	SessionsCopied          int       `json:"sessions_copied"`
	SubscriptionsCopied     int       `json:"subscriptions_copied"`
	QuotaUsageCopied        int       `json:"quota_usage_copied"`
	FinanceRowsCopied       int       `json:"finance_rows_copied"`
	UnsupportedRelations    int       `json:"unsupported_relations"`
	MigrationVersion        uint      `json:"target_migration_version"`
}

type generationUserPlan struct {
	User         businessUser
	UserRow      rawRow
	IdentityRows []rawRow
	Unsupported  int
	Status       string
	PlanSHA256   string
}

func validateGenerationPair(source, target string) error {
	s, t := generationNamePattern.FindStringSubmatch(source), generationNamePattern.FindStringSubmatch(target)
	if len(s) != 3 || len(t) != 3 || s[1] != t[1] {
		return errors.New("source and target must be versioned generations in the same environment")
	}
	var sourceVersion, targetVersion int
	if _, err := fmt.Sscanf(s[2], "%d", &sourceVersion); err != nil {
		return errors.New("source generation version is invalid")
	}
	if _, err := fmt.Sscanf(t[2], "%d", &targetVersion); err != nil || targetVersion != sourceVersion+1 {
		return errors.New("target must be the immediately following generation")
	}
	return nil
}

func validateGenerationOptions(options GenerationUserOptions) (map[string]businessTable, businessTable, businessTable, error) {
	var userTable, identityTable businessTable
	if err := validateGenerationPair(options.SourceGeneration, options.TargetGeneration); err != nil {
		return nil, userTable, identityTable, err
	}
	if options.Environment != "prod" || options.SchemaSHA256 == "" || options.BillingSHA256 != FullBusinessBillingSHA256 || options.ExpectedTargetVersion == 0 {
		return nil, userTable, identityTable, errors.New("generation user operation requires prod, reviewed schema hashes and an exact target version")
	}
	_, manifest, err := schema.NativeArtifact()
	if err != nil || manifest.SchemaSHA256 != options.SchemaSHA256 {
		return nil, userTable, identityTable, errors.New("generation user operation schema hash differs from the reviewed native artifact")
	}
	tables, _, err := fullBusinessContract()
	if err != nil {
		return nil, userTable, identityTable, err
	}
	userTable, ok := tables["users"]
	if !ok {
		return nil, userTable, identityTable, errors.New("reviewed users contract is missing")
	}
	identityTable, ok = tables["identities"]
	if !ok {
		return nil, userTable, identityTable, errors.New("reviewed OAuth identities contract is missing")
	}
	return tables, userTable, identityTable, nil
}

func beginGenerationUserSnapshot(ctx context.Context, dsn string, source bool) (*sql.DB, *sql.Tx, error) {
	db, err := openDB(ctx, dsn)
	if err != nil {
		return nil, nil, errors.New("generation database connection failed")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, nil, errors.New("cannot start generation read-only snapshot")
	}
	if _, err = tx.ExecContext(ctx, `SET LOCAL default_transaction_read_only=on; SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		tx.Rollback()
		db.Close()
		return nil, nil, errors.New("cannot enforce generation read-only snapshot")
	}
	if source {
		if err = businessSourceRole(ctx, tx); err != nil {
			tx.Rollback()
			db.Close()
			return nil, nil, err
		}
	}
	return db, tx, nil
}

func validateGenerationTarget(ctx context.Context, tx *sql.Tx, options GenerationUserOptions, userTable, identityTable businessTable) (uint, error) {
	var dbName string
	var pgVersion, count int
	var version uint
	var dirty bool
	if err := tx.QueryRowContext(ctx, `SELECT current_database(),current_setting('server_version_num')::int`).Scan(&dbName, &pgVersion); err != nil || dbName != "account" || pgVersion < 170000 || pgVersion >= 180000 {
		return 0, errors.New("generation target must be the PostgreSQL17 account database")
	}
	if err := tx.QueryRowContext(ctx, `SELECT count(*),COALESCE(min(version),0),COALESCE(bool_or(dirty),true) FROM public.schema_migrations`).Scan(&count, &version, &dirty); err != nil || count != 1 || dirty || version != options.ExpectedTargetVersion {
		return 0, errors.New("generation target migration checkpoint differs or is dirty")
	}
	if err := verifyCatalogCheckpoint(ctx, tx, version); err != nil {
		return 0, err
	}
	if err := validateBusinessTarget(ctx, tx, "users", userTable); err != nil {
		return 0, err
	}
	if err := validateBusinessTarget(ctx, tx, "identities", identityTable); err != nil {
		return 0, err
	}
	return version, nil
}

func validateGenerationSource(ctx context.Context, tx *sql.Tx, tables map[string]businessTable, userTable, identityTable businessTable) (map[string]ColumnDefinition, map[string]bool, error) {
	scope, err := businessScope(ctx, tx, tables, true)
	if err != nil {
		return nil, nil, err
	}
	var userColumns map[string]ColumnDefinition
	for name, table := range map[string]businessTable{"users": userTable, "identities": identityTable} {
		columns, err := catalogColumns(ctx, tx, name)
		if err != nil || validateBusinessSourceColumns(name, columns, table) != nil {
			return nil, nil, fmt.Errorf("source public.%s schema differs from the reviewed core identity contract", name)
		}
		if err = businessSourceVisibility(ctx, tx, name); err != nil {
			return nil, nil, err
		}
		if name == "users" {
			userColumns = columnsByName(columns)
		}
	}
	if userColumns == nil {
		return nil, nil, errors.New("source identity schema validation failed")
	}
	return userColumns, scope, nil
}

func columnsByName(columns []ColumnDefinition) map[string]ColumnDefinition {
	result := make(map[string]ColumnDefinition, len(columns))
	for _, column := range columns {
		result[column.Name] = column
	}
	return result
}

func generationUserDiff(source, target map[string]businessUser) GenerationUserDiff {
	diff := GenerationUserDiff{}
	for key, sourceUser := range source {
		targetUser, ok := target[key]
		if !ok {
			diff.SourceOnlyUsers++
			continue
		}
		if sourceUser.ID != targetUser.ID {
			diff.UserIDMismatches++
		}
		if sourceUser.Email != targetUser.Email {
			diff.EmailMismatches++
		}
		if sourceUser.Proxy != targetUser.Proxy {
			diff.ProxyUUIDMismatches++
		}
		if sourceUser.PasswordHash != targetUser.PasswordHash {
			diff.PasswordHashMismatches++
		}
	}
	for key := range target {
		if _, ok := source[key]; !ok {
			diff.TargetOnlyUsers++
		}
	}
	diff.Equal = diff.SourceOnlyUsers == 0 && diff.TargetOnlyUsers == 0 && diff.UserIDMismatches == 0 &&
		diff.EmailMismatches == 0 && diff.ProxyUUIDMismatches == 0 && diff.PasswordHashMismatches == 0
	return diff
}

func generationUserIDHash(id string) string {
	sum := sha256.Sum256([]byte("accounts-generation-user-id-v1:" + id))
	return hex.EncodeToString(sum[:])
}

func generationUserSet(users map[string]businessUser) ([]string, string) {
	hashes := make([]string, 0, len(users))
	for _, user := range users {
		hashes = append(hashes, generationUserIDHash(user.ID))
	}
	sort.Strings(hashes)
	return hashes, hashString(strings.Join(hashes, "\n"))
}

func hashString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func baselineChecksum(baseline GenerationUserBaseline) (string, error) {
	baseline.BaselineSHA256 = ""
	data, err := json.Marshal(baseline)
	if err != nil {
		return "", errors.New("cannot encode generation user baseline")
	}
	return hashString(string(data)), nil
}

func validateGenerationUserBaseline(baseline GenerationUserBaseline, options GenerationUserOptions) error {
	if baseline.Format != 1 || baseline.Algorithm != "sha256-generation-user-id-v1" ||
		baseline.SourceGeneration != options.SourceGeneration || baseline.TargetGeneration != options.TargetGeneration ||
		baseline.CapturedAt.IsZero() || baseline.SourceUserCount != len(baseline.SourceUserIDHashes) ||
		baseline.IdentityDiff.Equal != true {
		return errors.New("generation user baseline is incomplete or belongs to a different generation pair")
	}
	for i, value := range baseline.SourceUserIDHashes {
		if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(value) || (i > 0 && baseline.SourceUserIDHashes[i-1] >= value) {
			return errors.New("generation user baseline identity set is invalid")
		}
	}
	setHash := hashString(strings.Join(baseline.SourceUserIDHashes, "\n"))
	checksum, err := baselineChecksum(baseline)
	if err != nil || setHash != baseline.SourceUserSetSHA256 || checksum != baseline.BaselineSHA256 {
		return errors.New("generation user baseline checksum does not match")
	}
	return nil
}

func readGenerationUserSets(ctx context.Context, sourceDSN, targetDSN string, options GenerationUserOptions) (map[string]businessUser, map[string]businessUser, time.Time, uint, func(), error) {
	tables, userTable, identityTable, err := validateGenerationOptions(options)
	_ = tables
	if err != nil {
		return nil, nil, time.Time{}, 0, nil, err
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return nil, nil, time.Time{}, 0, nil, err
	}
	sourceDB, sourceTx, err := beginGenerationUserSnapshot(ctx, sourceDSN, true)
	if err != nil {
		return nil, nil, time.Time{}, 0, nil, err
	}
	targetDB, targetTx, err := beginGenerationUserSnapshot(ctx, targetDSN, false)
	if err != nil {
		sourceTx.Rollback()
		sourceDB.Close()
		return nil, nil, time.Time{}, 0, nil, err
	}
	started := time.Now().UTC()
	_, _, err = validateGenerationSource(ctx, sourceTx, tables, userTable, identityTable)
	if err != nil {
		targetTx.Rollback()
		sourceTx.Rollback()
		targetDB.Close()
		sourceDB.Close()
		return nil, nil, time.Time{}, 0, nil, err
	}
	version, err := validateGenerationTarget(ctx, targetTx, options, userTable, identityTable)
	if err != nil {
		targetTx.Rollback()
		sourceTx.Rollback()
		targetDB.Close()
		sourceDB.Close()
		return nil, nil, time.Time{}, 0, nil, err
	}
	sourceUsers, err := businessUsers(ctx, sourceTx)
	if err == nil {
		var targetUsers map[string]businessUser
		targetUsers, err = businessUsers(ctx, targetTx)
		if err == nil {
			cleanup := func() {
				targetTx.Rollback()
				sourceTx.Rollback()
				targetDB.Close()
				sourceDB.Close()
			}
			return sourceUsers, targetUsers, started, version, cleanup, nil
		}
	}
	targetTx.Rollback()
	sourceTx.Rollback()
	targetDB.Close()
	sourceDB.Close()
	return nil, nil, time.Time{}, 0, nil, errors.New("cannot read complete source/target user identities")
}

// CaptureGenerationUserBaseline records the stable source UUID set and refuses
// to create a cutover boundary until source and target identities are exact.
func CaptureGenerationUserBaseline(ctx context.Context, sourceDSN, targetDSN string, options GenerationUserOptions) (GenerationUserBaseline, error) {
	sourceUsers, targetUsers, started, _, cleanup, err := readGenerationUserSets(ctx, sourceDSN, targetDSN, options)
	if err != nil {
		return GenerationUserBaseline{}, err
	}
	defer cleanup()
	diff := generationUserDiff(sourceUsers, targetUsers)
	if !diff.Equal {
		return GenerationUserBaseline{}, errors.New("cannot record cutover baseline while user identity differences remain")
	}
	hashes, setHash := generationUserSet(sourceUsers)
	baseline := GenerationUserBaseline{Format: 1, Algorithm: "sha256-generation-user-id-v1",
		SourceGeneration: options.SourceGeneration, TargetGeneration: options.TargetGeneration,
		CapturedAt: started, SourceUserCount: len(sourceUsers), TargetUserCount: len(targetUsers),
		SourceUserIDHashes: hashes, SourceUserSetSHA256: setHash, IdentityDiff: diff}
	baseline.BaselineSHA256, err = baselineChecksum(baseline)
	if err != nil {
		return GenerationUserBaseline{}, err
	}
	return baseline, nil
}

// DetectLateGenerationUsers compares the current source UUID set to the saved
// pre-cutover baseline. It does not infer registration from created_at.
func DetectLateGenerationUsers(ctx context.Context, sourceDSN, targetDSN string, options GenerationUserOptions, baseline GenerationUserBaseline) (LateUsersAudit, error) {
	if err := validateGenerationUserBaseline(baseline, options); err != nil {
		return LateUsersAudit{}, err
	}
	sourceUsers, targetUsers, observed, _, cleanup, err := readGenerationUserSets(ctx, sourceDSN, targetDSN, options)
	if err != nil {
		return LateUsersAudit{}, err
	}
	defer cleanup()
	baselineSet := make(map[string]bool, len(baseline.SourceUserIDHashes))
	for _, idHash := range baseline.SourceUserIDHashes {
		baselineSet[idHash] = true
	}
	lateIDs := make(map[string]bool)
	for _, user := range sourceUsers {
		idHash := generationUserIDHash(user.ID)
		if !baselineSet[idHash] {
			lateIDs[idHash] = true
		}
	}
	lateMissing := 0
	for key, user := range sourceUsers {
		if !lateIDs[generationUserIDHash(user.ID)] {
			continue
		}
		target, ok := targetUsers[key]
		if !ok || target.ID != user.ID || target.Proxy != user.Proxy || target.Email != user.Email {
			lateMissing++
		}
	}
	diff := generationUserDiff(sourceUsers, targetUsers)
	anomaly := len(lateIDs) > 0 || !diff.Equal
	result := "clean"
	if anomaly {
		result = "source_write_isolation_anomaly"
	}
	return LateUsersAudit{Format: 1, SourceGeneration: options.SourceGeneration, TargetGeneration: options.TargetGeneration,
		BaselineCapturedAt: baseline.CapturedAt, ObservedAt: observed, BaselineSourceUserCount: baseline.SourceUserCount,
		CurrentSourceUserCount: len(sourceUsers), CurrentTargetUserCount: len(targetUsers),
		SourceUsersAbsentFromBaseline: len(lateIDs), LateSourceUsersMissingTarget: lateMissing,
		IdentityDiff: diff, SourceWriteIsolationAnomaly: anomaly, Result: result}, nil
}

func canonicalRowHash(row rawRow) (string, error) {
	data, err := json.Marshal(row)
	if err != nil {
		return "", errors.New("cannot hash reviewed account row")
	}
	return hashString(string(data)), nil
}

func userRelatedColumns(name string, table businessTable) []string {
	set := map[string]bool{}
	available := map[string]bool{}
	for _, column := range table.Columns {
		available[column.Name] = true
	}
	for _, column := range businessUserReferences[name] {
		if available[column] {
			set[column] = true
		}
	}
	for _, column := range []string{"account_uuid", "user_uuid"} {
		for _, definition := range table.Columns {
			if definition.Name == column {
				set[column] = true
			}
		}
	}
	for _, fk := range table.ForeignKeys {
		if fk.Parent != "users" {
			continue
		}
		for i, parentColumn := range fk.ParentColumns {
			if parentColumn == "uuid" {
				set[fk.Columns[i]] = true
			}
		}
	}
	columns := make([]string, 0, len(set))
	for column := range set {
		columns = append(columns, column)
	}
	sort.Strings(columns)
	return columns
}

func userRelatedCount(ctx context.Context, tx *sql.Tx, name string, table businessTable, userID string) (int, error) {
	var total int
	for _, column := range userRelatedColumns(name, table) {
		var count int
		query := "SELECT count(*) FROM public." + quoteBusiness(name) + " WHERE " + quoteBusiness(column) + "::text=$1"
		if err := tx.QueryRowContext(ctx, query, userID).Scan(&count); err != nil {
			return 0, errors.New("cannot inspect reviewed user associations")
		}
		total += count
	}
	return total, nil
}

func loadSingleBusinessRow(ctx context.Context, tx *sql.Tx, name, idColumn, id string) (rawRow, bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT to_jsonb(t)::text FROM public."+quoteBusiness(name)+" t WHERE "+quoteBusiness(idColumn)+"::text=$1", id)
	if err != nil {
		return nil, false, errors.New("cannot inspect selected account row")
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	var text string
	if err := rows.Scan(&text); err != nil {
		return nil, false, errors.New("cannot decode selected account row")
	}
	var row rawRow
	if err = json.Unmarshal([]byte(text), &row); err != nil {
		return nil, false, errors.New("cannot decode selected account row")
	}
	if rows.Next() {
		return nil, false, errors.New("selected account identifier is ambiguous")
	}
	if err = rows.Err(); err != nil {
		return nil, false, errors.New("cannot finish selected account read")
	}
	return row, true, nil
}

func loadIdentityRows(ctx context.Context, tx *sql.Tx, userID string) ([]rawRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT to_jsonb(i)::text FROM public.identities i WHERE user_uuid=$1::uuid ORDER BY uuid`, userID)
	if err != nil {
		return nil, errors.New("cannot inspect OAuth identities")
	}
	defer rows.Close()
	result := []rawRow{}
	for rows.Next() {
		var text string
		if rows.Scan(&text) != nil {
			return nil, errors.New("cannot decode OAuth identity")
		}
		var row rawRow
		if json.Unmarshal([]byte(text), &row) != nil {
			return nil, errors.New("cannot decode OAuth identity")
		}
		result = append(result, row)
	}
	if rows.Err() != nil {
		return nil, errors.New("cannot finish OAuth identity read")
	}
	return result, nil
}

func rowSetHash(rows []rawRow) (string, error) {
	values := make([]string, 0, len(rows))
	for _, row := range rows {
		digest, err := canonicalRowHash(row)
		if err != nil {
			return "", err
		}
		values = append(values, digest)
	}
	sort.Strings(values)
	return hashString(strings.Join(values, "\n")), nil
}

func unsupportedUserAssociations(ctx context.Context, tx *sql.Tx, tables map[string]businessTable, scope map[string]bool, userID string) (int, error) {
	unsupported := 0
	for name, table := range tables {
		if name == "users" || name == "identities" || name == "sessions" || !scope[name] {
			continue
		}
		if len(userRelatedColumns(name, table)) == 0 {
			continue
		}
		count, err := userRelatedCount(ctx, tx, name, table, userID)
		if err != nil {
			return 0, err
		}
		unsupported += count
	}
	return unsupported, nil
}

func planSingleGenerationUser(ctx context.Context, sourceTx, targetTx *sql.Tx, options SingleGenerationUserOptions,
	tables map[string]businessTable, userTable, identityTable businessTable, sourceColumns map[string]ColumnDefinition) (generationUserPlan, error) {
	var plan generationUserPlan
	parsedID, err := uuid.Parse(options.SourceUserID)
	if err != nil || parsedID == uuid.Nil || parsedID.String() != options.SourceUserID {
		return plan, errors.New("source user ID must be a canonical UUID")
	}
	users, err := businessUsers(ctx, sourceTx)
	if err != nil {
		return plan, errors.New("source user identity set is ambiguous")
	}
	var selected businessUser
	found := false
	for _, user := range users {
		if user.ID == options.SourceUserID {
			selected = user
			found = true
			break
		}
	}
	if !found {
		return plan, errors.New("selected source user does not exist")
	}
	plan.User = selected
	plan.UserRow, found, err = loadSingleBusinessRow(ctx, sourceTx, "users", "uuid", options.SourceUserID)
	if err != nil || !found {
		return plan, errors.New("selected source user row is unavailable")
	}
	if err = projectBusinessRow("users", plan.UserRow, columnsToSlice(sourceColumns), userTable, map[string]string{selected.ID: selected.ID}); err != nil {
		return plan, errors.New("source user row differs from reviewed target contract")
	}
	plan.IdentityRows, err = loadIdentityRows(ctx, sourceTx, selected.ID)
	if err != nil {
		return plan, err
	}
	for i := range plan.IdentityRows {
		if err = projectBusinessRow("identities", plan.IdentityRows[i], tableColumnDefinitions(identityTable), identityTable, map[string]string{selected.ID: selected.ID}); err != nil {
			return plan, errors.New("source OAuth identity differs from reviewed target contract")
		}
	}
	_, scope, err := validateGenerationSource(ctx, sourceTx, tables, userTable, identityTable)
	if err != nil {
		return plan, err
	}
	plan.Unsupported, err = unsupportedUserAssociations(ctx, sourceTx, tables, scope, selected.ID)
	if err != nil {
		return plan, err
	}
	targetUsers, err := businessUsers(ctx, targetTx)
	if err != nil {
		return plan, errors.New("target user identity set is ambiguous")
	}
	selectedKey := strings.ToLower(strings.TrimSpace(selected.Email))
	matched, emailExists := targetUsers[selectedKey]
	for _, targetUser := range targetUsers {
		if targetUser.ID == selected.ID && (targetUser.Email != selected.Email || targetUser.Proxy != selected.Proxy) {
			plan.Status = "identity_conflict"
			return sealSingleUserPlan(plan)
		}
		if targetUser.Proxy == selected.Proxy && targetUser.ID != selected.ID {
			plan.Status = "identity_conflict"
			return sealSingleUserPlan(plan)
		}
	}
	if emailExists {
		if matched.ID != selected.ID || matched.Email != selected.Email || matched.Proxy != selected.Proxy {
			plan.Status = "identity_conflict"
			return sealSingleUserPlan(plan)
		}
		plan.Status = "already_present_preserved"
	} else if plan.Unsupported > 0 {
		plan.Status = "unsupported_associations"
	} else {
		plan.Status = "missing_ready"
	}
	if selected.ID != "" {
		if _, idExists := findUserByID(targetUsers, selected.ID); idExists && !emailExists {
			plan.Status = "identity_conflict"
		}
	}
	if plan.Status == "missing_ready" {
		// Every provider/external identity and identity UUID is checked before
		// planning an insert; uniqueness constraints remain enabled on apply.
		for _, identity := range plan.IdentityRows {
			identityID := rowString(identity, "uuid")
			provider, externalID := rowString(identity, "provider"), rowString(identity, "external_id")
			var collisions int
			if err = targetTx.QueryRowContext(ctx, `SELECT count(*) FROM public.identities WHERE uuid::text=$1 OR (provider=$2 AND external_id=$3)`, identityID, provider, externalID).Scan(&collisions); err != nil || collisions != 0 {
				plan.Status = "identity_conflict"
				break
			}
		}
	}
	return sealSingleUserPlan(plan)
}

func sealSingleUserPlan(plan generationUserPlan) (generationUserPlan, error) {
	userHash, err := canonicalRowHash(plan.UserRow)
	if err != nil {
		return plan, err
	}
	identityHash, err := rowSetHash(plan.IdentityRows)
	if err != nil {
		return plan, err
	}
	material := strings.Join([]string{generationUserIDHash(plan.User.ID), userHash, identityHash,
		fmt.Sprintf("%d", plan.Unsupported), plan.Status}, ":")
	plan.PlanSHA256 = hashString(material)
	return plan, nil
}

func findUserByID(users map[string]businessUser, id string) (businessUser, bool) {
	for _, user := range users {
		if user.ID == id {
			return user, true
		}
	}
	return businessUser{}, false
}

func columnsToSlice(columns map[string]ColumnDefinition) []ColumnDefinition {
	result := make([]ColumnDefinition, 0, len(columns))
	for _, column := range columns {
		result = append(result, column)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func tableColumnDefinitions(table businessTable) []ColumnDefinition {
	result := make([]ColumnDefinition, 0, len(table.Columns))
	for _, column := range table.Columns {
		result = append(result, ColumnDefinition{Name: column.Name, Type: column.Type, Nullable: column.Nullable, Generated: column.Generated})
	}
	return result
}

// SyncSingleGenerationUser previews by default. Apply requires a reviewed
// source-isolation assertion, an exact repeated plan hash and explicit ID
// confirmation. It inserts only a missing core identity and OAuth identities.
func SyncSingleGenerationUser(ctx context.Context, sourceDSN, targetDSN string, options SingleGenerationUserOptions) (SingleGenerationUserReceipt, error) {
	receipt := SingleGenerationUserReceipt{Format: 1, ReceiptID: uuid.NewString(),
		SourceGeneration: options.SourceGeneration, TargetGeneration: options.TargetGeneration,
		SourceUserIDSHA256: generationUserIDHash(options.SourceUserID), ObservedAt: time.Now().UTC(),
		SourceReadOnly: true, TargetWrites: false}
	tables, userTable, identityTable, err := validateGenerationOptions(options.GenerationUserOptions)
	if err != nil {
		return receipt, err
	}
	if err = validateBusinessConnections(sourceDSN, targetDSN); err != nil {
		return receipt, err
	}
	sourceDB, sourceTx, err := beginGenerationUserSnapshot(ctx, sourceDSN, true)
	if err != nil {
		return receipt, err
	}
	defer sourceDB.Close()
	defer sourceTx.Rollback()
	sourceColumns, _, err := validateGenerationSource(ctx, sourceTx, tables, userTable, identityTable)
	if err != nil {
		return receipt, err
	}
	targetDB, err := openDB(ctx, targetDSN)
	if err != nil {
		return receipt, errors.New("generation target connection failed")
	}
	defer targetDB.Close()
	targetTx, err := targetDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable, ReadOnly: !options.Apply})
	if err != nil {
		return receipt, errors.New("cannot start generation target transaction")
	}
	defer targetTx.Rollback()
	if _, err = targetTx.ExecContext(ctx, `SET LOCAL timezone='UTC'; SET LOCAL datestyle='ISO,YMD'; SET LOCAL statement_timeout='5min'; SET LOCAL lock_timeout='15s'; SET LOCAL idle_in_transaction_session_timeout='5min'`); err != nil {
		return receipt, errors.New("cannot enforce target transaction budgets")
	}
	receipt.MigrationVersion, err = validateGenerationTarget(ctx, targetTx, options.GenerationUserOptions, userTable, identityTable)
	if err != nil {
		return receipt, err
	}
	if options.Apply {
		if !options.SourceWritersPaused || options.ConfirmSourceUserID != options.SourceUserID || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(options.ExpectedPlanSHA256) {
			return receipt, errors.New("apply requires isolated source writers, explicit matching user-ID confirmation and a reviewed plan checksum")
		}
		if _, err = targetTx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1::bigint)`, migrationAdvisoryLockKey); err != nil {
			return receipt, errors.New("cannot acquire single-user reconciliation lock")
		}
		if _, err = targetTx.ExecContext(ctx, `LOCK TABLE public.users, public.identities IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return receipt, errors.New("cannot lock target identity tables")
		}
	}
	plan, err := planSingleGenerationUser(ctx, sourceTx, targetTx, options, tables, userTable, identityTable, sourceColumns)
	if err != nil {
		return receipt, err
	}
	receipt.PlanSHA256 = plan.PlanSHA256
	receipt.UnsupportedRelations = plan.Unsupported
	switch plan.Status {
	case "already_present_preserved":
		receipt.Result = plan.Status
		if err = targetTx.Commit(); err != nil {
			return receipt, errors.New("target read-only transaction did not complete")
		}
		return receipt, nil
	case "identity_conflict":
		receipt.Result = plan.Status
		return receipt, errors.New("selected source account collides with a target user ID, email, Proxy UUID or OAuth identity")
	case "unsupported_associations":
		receipt.Result = plan.Status
		return receipt, errors.New("selected source account has subscription, quota, usage, financial or other linked records outside the approved core sync scope")
	case "missing_ready":
	default:
		return receipt, errors.New("selected source account is not eligible for core synchronization")
	}
	if !options.Apply {
		receipt.Result = "preview_missing_user"
		if err = targetTx.Commit(); err != nil {
			return receipt, errors.New("target read-only preview did not complete")
		}
		return receipt, nil
	}
	if options.ExpectedPlanSHA256 != plan.PlanSHA256 {
		return receipt, errors.New("source or target changed since the reviewed single-user preview")
	}
	if err = insertBusiness(ctx, targetTx, "users", userTable, []rawRow{plan.UserRow}); err != nil {
		return receipt, errors.New("single-user insert failed; target transaction rolled back")
	}
	if len(plan.IdentityRows) > 0 {
		if err = insertBusiness(ctx, targetTx, "identities", identityTable, plan.IdentityRows); err != nil {
			return receipt, errors.New("OAuth identity insert failed; target transaction rolled back")
		}
	}
	var insertedID, insertedEmail, insertedProxy string
	if err = targetTx.QueryRowContext(ctx, `SELECT uuid::text,email,proxy_uuid::text FROM public.users WHERE uuid=$1::uuid`, plan.User.ID).Scan(&insertedID, &insertedEmail, &insertedProxy); err != nil || insertedID != plan.User.ID || insertedEmail != plan.User.Email || insertedProxy != plan.User.Proxy {
		return receipt, errors.New("post-insert user identity verification failed; target transaction rolled back")
	}
	var identityCount int
	if err = targetTx.QueryRowContext(ctx, `SELECT count(*) FROM public.identities WHERE user_uuid=$1::uuid`, plan.User.ID).Scan(&identityCount); err != nil || identityCount != len(plan.IdentityRows) {
		return receipt, errors.New("post-insert OAuth relationship verification failed; target transaction rolled back")
	}
	if err = targetTx.Commit(); err != nil {
		return receipt, errors.New("single-user transaction did not commit")
	}
	receipt.Result = "inserted"
	receipt.TargetWrites = true
	receipt.UserRowsInserted = 1
	receipt.OAuthIdentityRowsCopied = len(plan.IdentityRows)
	return receipt, nil
}
