package api

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

// Commercial subscription facts are stored in PostgreSQL; the browser only
// submits CSV and displays the preview. Entitlements and records commit together.
func (h *handler) subscriptionCSVUpdates(ctx context.Context, source string, reconcile bool) ([]adminPlanGroupUpdate, error) {
	updates, err := h.subscriptionCSVUpdatesValidated(ctx, source, reconcile)
	if err == nil || len(source) > 2*1024*1024 {
		return updates, err
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(source, "\uFEFF")))
	reader.TrimLeadingSpace = true
	rows, parseErr := reader.ReadAll()
	if parseErr != nil || len(rows) < 2 {
		return nil, err
	}
	issues := []string{}
	seen := map[string]bool{}
	emailColumn := -1
	for i, header := range rows[0] {
		if strings.EqualFold(strings.TrimSpace(header), "email") || strings.TrimSpace(header) == "邮箱" {
			emailColumn = i
		}
	}
	for i, row := range rows[1:] {
		var body bytes.Buffer
		writer := csv.NewWriter(&body)
		_ = writer.WriteAll([][]string{rows[0], row})
		if emailColumn >= 0 && emailColumn < len(row) {
			email := strings.ToLower(strings.TrimSpace(row[emailColumn]))
			if seen[email] {
				issues = append(issues, fmt.Sprintf("row %d: duplicate email %s", i+2, email))
				continue
			}
			seen[email] = true
		}
		if _, rowErr := h.subscriptionCSVUpdatesValidated(ctx, body.String(), false); rowErr != nil {
			issues = append(issues, fmt.Sprintf("row %d: %s", i+2, strings.TrimPrefix(rowErr.Error(), "row 2: ")))
		}
	}
	if len(issues) > 0 {
		return nil, errors.New(strings.Join(issues, "\n"))
	}
	return nil, err
}

func (h *handler) subscriptionCSVUpdatesValidated(ctx context.Context, source string, reconcile bool) ([]adminPlanGroupUpdate, error) {
	if len(source) > 2*1024*1024 {
		return nil, errors.New("CSV exceeds 2 MiB")
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(source, "\uFEFF")))
	reader.TrimLeadingSpace = true
	rows, err := reader.ReadAll()
	if err != nil || len(rows) < 2 {
		return nil, errors.New("CSV must contain a header and at least one data row")
	}
	headers := map[string]int{}
	for i, header := range rows[0] {
		headers[strings.ToLower(strings.TrimSpace(header))] = i
	}
	cell := func(row []string, names ...string) string {
		for _, name := range names {
			if index, ok := headers[name]; ok && index < len(row) {
				value := strings.TrimSpace(row[index])
				// Reverse only our formula-escape prefix, never arbitrary apostrophes.
				if escaped := strings.TrimLeft(strings.TrimPrefix(value, "'"), " \t\r\n"); strings.HasPrefix(value, "'") && len(escaped) > 0 && strings.ContainsRune("=+-@", rune(escaped[0])) {
					value = value[1:]
				}
				return value
			}
		}
		return ""
	}
	if _, ok := headers["邮箱"]; !ok {
		if _, ok := headers["email"]; !ok {
			return nil, errors.New("CSV requires an email/邮箱 column")
		}
	}
	users, err := h.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	byEmail := map[string]store.User{}
	for _, user := range users {
		if user.Email != "" {
			key := strings.ToLower(strings.TrimSpace(user.Email))
			if _, duplicate := byEmail[key]; duplicate {
				return nil, errors.New("database contains ambiguous email addresses")
			}
			byEmail[key] = user
		}

	}
	seen := map[string]bool{}
	updates := make([]adminPlanGroupUpdate, 0, len(rows)-1)
	now := time.Now().UTC()
	for line, row := range rows[1:] {
		if len(updates) >= maxAdminPlanGroupBatch {
			return nil, errors.New("CSV contains more than 500 users")
		}
		email := strings.ToLower(cell(row, "邮箱", "email"))
		if email == "" {
			return nil, fmt.Errorf("row %d: email is required", line+2)
		}
		user, ok := byEmail[email]
		if !ok {
			return nil, fmt.Errorf("row %d: existing user was not found", line+2)
		}
		if h.isRootAccount(&user) {
			return nil, fmt.Errorf("row %d: root account cannot be imported", line+2)
		}
		if seen[user.ID] {
			return nil, fmt.Errorf("row %d: duplicate user", line+2)
		}
		seen[user.ID] = true
		if isTeamSubscriptionUser(user.Groups) {
			return nil, fmt.Errorf("row %d: team users require team management", line+2)
		}
		from, err := subscriptionImportDate(cell(row, "订阅开始", "subscription start", "valid_from"))
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid start date", line+2)
		}
		until, err := subscriptionImportDate(cell(row, "订阅结束", "subscription end", "valid_until"))
		if err != nil || from != nil && until != nil && until.Before(*from) {
			return nil, fmt.Errorf("row %d: invalid end date", line+2)
		}
		existing, recordErr := h.store.GetSubscriptionRecord(ctx, user.ID)
		if recordErr != nil && !errors.Is(recordErr, store.ErrUserNotFound) {
			return nil, recordErr
		}
		if from == nil {
			from = user.SubscriptionValidFrom
			if existing != nil && existing.ValidFrom != nil {
				from = existing.ValidFrom
			}
		}
		if until == nil {
			until = user.SubscriptionValidUntil
			if existing != nil && existing.ValidUntil != nil {
				until = existing.ValidUntil
			}
		}
		if from != nil && until != nil && until.Before(*from) {
			return nil, fmt.Errorf("row %d: invalid merged validity", line+2)
		}
		monthly, err := subscriptionImportBytes(cell(row, "月配额", "monthly quota", "monthly_quota_bytes"))
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid monthly quota", line+2)
		}
		annual, err := subscriptionImportBytes(cell(row, "年配额", "annual quota", "annual_quota_bytes"))
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid annual quota", line+2)
		}
		feeText := cell(row, "费用", "fee", "fee_minor")
		factor := float64(100)
		if _, minor := headers["fee_minor"]; minor {
			factor = 1
		}
		fee, err := subscriptionImportNumber(strings.TrimSuffix(strings.TrimPrefix(feeText, "¥"), "元"), factor)
		if err != nil {
			return nil, fmt.Errorf("row %d: invalid fee", line+2)
		}
		payment := strings.ToLower(cell(row, "付费状态", "paid status", "payment_status"))
		kind := strings.ToLower(cell(row, "订阅类型", "subscription type", "subscription_type"))
		note := cell(row, "备注", "note")
		if existing != nil {
			if kind == "" {
				kind = existing.SubscriptionType
			}
			if payment == "" {
				payment = existing.PaymentStatus
			}
			if note == "" {
				note = existing.Note
			}
			if cell(row, "月配额", "monthly quota", "monthly_quota_bytes") == "" {
				monthly = existing.MonthlyQuotaBytes
			}
			if cell(row, "年配额", "annual quota", "annual_quota_bytes") == "" {
				annual = existing.AnnualQuotaBytes
			}
			if feeText == "" {
				fee = existing.FeeMinor
			}
		}
		if kind != "" && kind != "month" && kind != "year" && kind != "payg" {
			return nil, fmt.Errorf("row %d: invalid subscription type", line+2)
		}
		plan := strings.ToUpper(cell(row, "plan", "套餐"))
		paid := payment == "已付费" || payment == "paid" || payment == "已支付"
		if plan == "" {
			if paid {
				plan = store.BillingPlanPlus
			} else {
				plan = store.BillingPlanFree
			}
		}
		if _, err := planIDToQuotaGroup(plan); err != nil {
			return nil, fmt.Errorf("row %d: invalid plan", line+2)
		}
		// An expired or not-yet-started commercial subscription grants no paid access.
		if until != nil && until.Before(now.Truncate(24*time.Hour)) || from != nil && from.After(now) {
			plan = store.BillingPlanFree
		}
		record := &store.SubscriptionRecord{UserID: user.ID, SubscriptionType: kind, MonthlyQuotaBytes: monthly, AnnualQuotaBytes: annual, FeeMinor: fee, PaymentStatus: payment, Note: note, ValidFrom: from, ValidUntil: until}
		start, _ := json.Marshal(adminPlanDateString(from))
		end, _ := json.Marshal(adminPlanDateString(until))
		if from == nil {
			start = json.RawMessage("null")
		}
		if until == nil {
			end = json.RawMessage("null")
		}
		updates = append(updates, adminPlanGroupUpdate{UserID: user.ID, PlanID: plan, ValidFrom: start, ValidUntil: end, SubscriptionRecord: record})
	}
	if len(updates) == 0 {
		return nil, errors.New("CSV has no editable existing users")
	}
	if reconcile {
		for _, user := range users {
			if seen[user.ID] || h.isRootAccount(&user) || isTeamSubscriptionUser(user.Groups) {
				continue
			}
			// Removing membership is reversible: retain historical commercial facts,
			// validity and payment records. Only the effective plan/cohort changes.
			updates = append(updates, adminPlanGroupUpdate{UserID: user.ID, PlanID: store.BillingPlanFree})
		}
	}
	return updates, nil
}

func isTeamSubscriptionUser(groups []string) bool {
	for _, group := range groups {
		if strings.HasPrefix(group, "team:") || strings.HasPrefix(group, "team-admin:") || group == "access:dedicated-only" {
			return true
		}
	}
	return false
}
func subscriptionImportDate(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	for _, layout := range []string{"2006.1.2", "2006-1-2", "2006/1/2", time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return &parsed, nil
		}
	}
	return nil, errors.New("invalid date")
}
func subscriptionImportNumber(value string, factor float64) (int64, error) {
	if value == "" {
		return 0, nil
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	scaled := math.Round(number * factor)
	if err != nil || math.IsNaN(scaled) || math.IsInf(scaled, 0) || scaled < 0 || scaled >= float64(math.MaxInt64) {
		return 0, errors.New("invalid nonnegative amount")
	}
	return int64(scaled), nil
}
func subscriptionImportBytes(value string) (int64, error) {
	upper := strings.ToUpper(value)
	for _, unit := range []struct {
		suffix string
		factor float64
	}{{"GIB", 1 << 30}, {"GB", 1 << 30}, {"MIB", 1 << 20}, {"MB", 1 << 20}, {"B", 1}} {
		if strings.HasSuffix(upper, unit.suffix) {
			return subscriptionImportNumber(strings.TrimSuffix(upper, unit.suffix), unit.factor)
		}
	}
	return subscriptionImportNumber(value, 1)
}

func (h *handler) adminListSubscriptionRecords(c *gin.Context) {
	actor, ok := h.requireAdminPermission(c, permissionAdminSettingsWrite)
	if !ok {
		return
	}
	if !store.IsRootRole(actor.Role) {
		respondError(c, 403, "root_required", "only root can export subscription records")
		return
	}
	records, err := h.store.ListSubscriptionRecords(c.Request.Context())
	if err != nil {
		respondError(c, 500, "subscription_records_unavailable", "failed to load subscription records")
		return
	}
	users, err := h.store.ListUsers(c.Request.Context())
	if err != nil {
		respondError(c, 500, "users_unavailable", "failed to load users")
		return
	}
	byID := map[string]store.SubscriptionRecord{}
	for _, record := range records {
		byID[record.UserID] = record
	}
	c.Header("Cache-Control", "no-store")
	if c.Query("format") != "csv" {
		c.JSON(200, gin.H{"records": records})
		return
	}
	var body bytes.Buffer
	body.WriteString("\uFEFF")
	writer := csv.NewWriter(&body)
	_ = writer.Write([]string{"email", "username", "plan", "subscription_type", "monthly_quota_bytes", "annual_quota_bytes", "fee_minor", "payment_status", "valid_from", "valid_until", "note", "temporary_unlimited"})
	for _, user := range users {
		if h.isRootAccount(&user) || isTeamSubscriptionUser(user.Groups) {
			continue
		}
		record, exists := byID[user.ID]
		if !exists {
			continue
		}
		if !slices.Contains(user.Groups, store.SubscriptionListedGroup) && c.Query("includeHistory") != "true" {
			continue
		}
		profile, err := h.store.GetAccountBillingProfile(c.Request.Context(), user.ID)
		if err != nil && !errors.Is(err, store.ErrUserNotFound) {
			respondError(c, 500, "billing_profile_unavailable", "failed to load account plan")
			return
		}
		plan := existingPlanID(profile)
		if plan == "" {
			plan = store.BillingPlanFree
		}
		_ = writer.Write([]string{exportCSVCell(user.Email), exportCSVCell(user.Name), plan, exportCSVCell(record.SubscriptionType), strconv.FormatInt(record.MonthlyQuotaBytes, 10), strconv.FormatInt(record.AnnualQuotaBytes, 10), strconv.FormatInt(record.FeeMinor, 10), exportCSVCell(record.PaymentStatus), subscriptionCSVDate(record.ValidFrom), subscriptionCSVDate(record.ValidUntil), exportCSVCell(record.Note), strconv.FormatBool(store.HasSubscriptionUnlimitedOverride(&user, time.Now()))})
	}
	writer.Flush()
	if writer.Error() != nil {
		c.Status(500)
		return
	}
	c.Header("Content-Disposition", "attachment; filename=subscription-records.csv")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/csv; charset=utf-8", body.Bytes())
}
func exportCSVCell(value string) string {
	// Spreadsheet apps must not interpret operator notes as formulas.
	trimmed := strings.TrimLeft(value, " \t\r\n")
	if len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

func subscriptionCSVDate(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format("2006-01-02")
}
