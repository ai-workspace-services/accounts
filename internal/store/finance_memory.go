package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func financeID(id string) string {
	if strings.TrimSpace(id) == "" {
		return uuid.NewString()
	}
	return strings.TrimSpace(id)
}

func financeProvider(provider string) string {
	if provider = strings.ToLower(strings.TrimSpace(provider)); provider == "" {
		return "local"
	}
	return provider
}

func financeCurrency(currency string) string { return strings.ToUpper(strings.TrimSpace(currency)) }

func validFinanceCurrency(currency string) bool {
	if len(currency) != 3 {
		return false
	}
	for _, r := range currency {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func sameOptionalTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func financeJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return append(json.RawMessage(nil), raw...)
}

func financeRefundRequest(operation *FinanceOperation) (int64, string, error) {
	var payload struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
	}
	if err := json.Unmarshal(operation.Request, &payload); err != nil {
		return 0, "", err
	}
	payload.Currency = financeCurrency(payload.Currency)
	if payload.AmountMinor <= 0 || !validFinanceCurrency(payload.Currency) {
		return 0, "", fmt.Errorf("refund operation requires positive amount_minor and ISO currency")
	}
	return payload.AmountMinor, payload.Currency, nil
}

func financeOperationReservesRefund(operation *FinanceOperation) bool {
	return operation.OperationType == "refund" && operation.TargetType == "payment" && (operation.Status == FinanceOperationPending || operation.Status == FinanceOperationInProgress || operation.Status == FinanceOperationSucceeded || operation.Status == FinanceOperationReconciliationRequired)
}

func financeRefundMatchesOperation(operation *FinanceOperation, refund *FinanceRefund) bool {
	amount, currency, err := financeRefundRequest(operation)
	return err == nil && operation.Status == FinanceOperationSucceeded && operation.OperationType == "refund" && operation.TargetType == "payment" && operation.TargetID == refund.PaymentID && operation.Provider == refund.Provider && amount == refund.AmountMinor && currency == refund.Currency && (operation.ProviderOperationID == "" || operation.ProviderOperationID == refund.ProviderRefundID)
}

func (s *memoryStore) CreateFinanceInvoice(ctx context.Context, invoice *FinanceInvoice) (bool, error) {
	if invoice == nil || strings.TrimSpace(invoice.IdempotencyKey) == "" || strings.TrimSpace(invoice.AccountUUID) == "" || invoice.AmountMinor < 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *invoice
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.AccountUUID = strings.TrimSpace(copy.AccountUUID)
	copy.SubscriptionUUID = strings.TrimSpace(copy.SubscriptionUUID)
	copy.Provider = financeProvider(copy.Provider)
	copy.Currency = financeCurrency(copy.Currency)
	copy.ProviderInvoiceID = strings.TrimSpace(copy.ProviderInvoiceID)
	copy.Description = strings.TrimSpace(copy.Description)
	issuedAtProvided := !copy.IssuedAt.IsZero()
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	if copy.IssuedAt.IsZero() {
		copy.IssuedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byID[copy.AccountUUID]; !exists {
		return false, ErrFinanceRecordNotFound
	}
	if copy.SubscriptionUUID != "" {
		found := false
		for _, subscription := range s.subscriptions[copy.AccountUUID] {
			if subscription.ID == copy.SubscriptionUUID {
				found = true
				break
			}
		}
		if !found {
			return false, ErrFinanceRecordNotFound
		}
	}
	if id, exists := s.financeInvoiceKeys[copy.IdempotencyKey]; exists {
		old := s.financeInvoices[id]
		if old.AccountUUID != copy.AccountUUID || old.SubscriptionUUID != copy.SubscriptionUUID || old.Provider != copy.Provider || old.ProviderInvoiceID != copy.ProviderInvoiceID || old.AmountMinor != copy.AmountMinor || old.Currency != copy.Currency || old.Description != copy.Description || !sameOptionalTime(copy.DueAt, old.DueAt) || issuedAtProvided && !copy.IssuedAt.Equal(old.IssuedAt) {
			return false, ErrFinanceIdempotencyConflict
		}
		invoice.ID, invoice.CreatedAt = old.ID, old.CreatedAt
		return false, nil
	}
	if _, exists := s.financeInvoices[copy.ID]; exists {
		return false, ErrFinanceIdempotencyConflict
	}
	if copy.ProviderInvoiceID != "" {
		for _, existing := range s.financeInvoices {
			if existing.Provider == copy.Provider && existing.ProviderInvoiceID == copy.ProviderInvoiceID {
				return false, ErrFinanceIdempotencyConflict
			}
		}
	}
	copy.CreatedAt = time.Now().UTC()
	s.financeInvoices[copy.ID] = &copy
	s.financeInvoiceKeys[copy.IdempotencyKey] = copy.ID
	*invoice = copy
	return true, nil
}

func (s *memoryStore) GetFinanceInvoice(ctx context.Context, id string) (*FinanceInvoice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	invoice, ok := s.financeInvoices[strings.TrimSpace(id)]
	if !ok {
		return nil, ErrFinanceRecordNotFound
	}
	copy := *invoice
	return &copy, nil
}

func (s *memoryStore) ListFinanceInvoices(ctx context.Context, accountUUID, subscriptionUUID string, limit int) ([]FinanceInvoice, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]FinanceInvoice, 0)
	for _, invoice := range s.financeInvoices {
		if accountUUID != "" && invoice.AccountUUID != accountUUID || subscriptionUUID != "" && invoice.SubscriptionUUID != subscriptionUUID {
			continue
		}
		result = append(result, *invoice)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) RecordFinancePayment(ctx context.Context, payment *FinancePayment) (bool, error) {
	if payment == nil || strings.TrimSpace(payment.IdempotencyKey) == "" || payment.AmountMinor <= 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *payment
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.InvoiceID = strings.TrimSpace(copy.InvoiceID)
	copy.AccountUUID = strings.TrimSpace(copy.AccountUUID)
	copy.Provider = financeProvider(copy.Provider)
	copy.Currency = financeCurrency(copy.Currency)
	copy.ProviderPaymentID = strings.TrimSpace(copy.ProviderPaymentID)
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, exists := s.financePaymentKeys[copy.IdempotencyKey]; exists {
		old := s.financePayments[id]
		if old.InvoiceID != copy.InvoiceID || old.AmountMinor != copy.AmountMinor || old.Currency != copy.Currency || old.Provider != copy.Provider || old.ProviderPaymentID != copy.ProviderPaymentID || !copy.PaidAt.IsZero() && !copy.PaidAt.Equal(old.PaidAt) {
			return false, ErrFinanceIdempotencyConflict
		}
		payment.ID, payment.CreatedAt = old.ID, old.CreatedAt
		return false, nil
	}
	invoice, ok := s.financeInvoices[copy.InvoiceID]
	if !ok || invoice.AccountUUID != copy.AccountUUID || invoice.Currency != copy.Currency || invoice.AmountMinor != copy.AmountMinor {
		return false, ErrFinanceIdempotencyConflict
	}
	if _, exists := s.financePayments[copy.ID]; exists {
		return false, ErrFinanceIdempotencyConflict
	}
	if copy.ProviderPaymentID != "" {
		for _, existing := range s.financePayments {
			if existing.Provider == copy.Provider && existing.ProviderPaymentID == copy.ProviderPaymentID {
				return false, ErrFinanceIdempotencyConflict
			}
		}
	}
	if copy.PaidAt.IsZero() {
		copy.PaidAt = time.Now().UTC()
	}
	copy.CreatedAt = time.Now().UTC()
	s.financePayments[copy.ID] = &copy
	s.financePaymentKeys[copy.IdempotencyKey] = copy.ID
	*payment = copy
	return true, nil
}

func (s *memoryStore) ListFinancePayments(ctx context.Context, accountUUID string, limit int) ([]FinancePayment, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]FinancePayment, 0)
	for _, payment := range s.financePayments {
		if accountUUID != "" && payment.AccountUUID != accountUUID {
			continue
		}
		result = append(result, *payment)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) RecordFinanceRefund(ctx context.Context, refund *FinanceRefund) (bool, error) {
	if refund == nil || strings.TrimSpace(refund.IdempotencyKey) == "" || refund.AmountMinor <= 0 {
		return false, ErrFinanceIdempotencyConflict
	}
	copy := *refund
	copy.ID = financeID(copy.ID)
	copy.IdempotencyKey = strings.TrimSpace(copy.IdempotencyKey)
	copy.PaymentID = strings.TrimSpace(copy.PaymentID)
	copy.Provider = financeProvider(copy.Provider)
	copy.Currency = financeCurrency(copy.Currency)
	copy.ProviderRefundID = strings.TrimSpace(copy.ProviderRefundID)
	copy.Reason = strings.TrimSpace(copy.Reason)
	if !validFinanceCurrency(copy.Currency) {
		return false, ErrFinanceIdempotencyConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if id, exists := s.financeRefundKeys[copy.IdempotencyKey]; exists {
		old := s.financeRefunds[id]
		if old.PaymentID != copy.PaymentID || old.AmountMinor != copy.AmountMinor || old.Currency != copy.Currency || old.Provider != copy.Provider || old.ProviderRefundID != copy.ProviderRefundID || old.Reason != strings.TrimSpace(copy.Reason) || !copy.RefundedAt.IsZero() && !copy.RefundedAt.Equal(old.RefundedAt) {
			return false, ErrFinanceIdempotencyConflict
		}
		refund.ID, refund.CreatedAt = old.ID, old.CreatedAt
		return false, nil
	}
	if operationID, exists := s.financeOperationKeys[copy.IdempotencyKey]; exists {
		if !financeRefundMatchesOperation(s.financeOperations[operationID], &copy) {
			return false, ErrFinanceIdempotencyConflict
		}
	}
	payment, ok := s.financePayments[copy.PaymentID]
	if !ok || payment.Currency != copy.Currency {
		return false, ErrFinanceRecordNotFound
	}
	var total int64
	for _, existing := range s.financeRefunds {
		if existing.PaymentID == copy.PaymentID {
			total += existing.AmountMinor
		}
	}
	for _, operation := range s.financeOperations {
		if financeOperationReservesRefund(operation) && operation.TargetID == copy.PaymentID && operation.IdempotencyKey != copy.IdempotencyKey && s.financeRefundKeys[operation.IdempotencyKey] == "" {
			amount, _, err := financeRefundRequest(operation)
			if err == nil {
				total += amount
			}
		}
	}
	if copy.AmountMinor > payment.AmountMinor-total {
		return false, ErrFinanceRefundExceedsPayment
	}
	if _, exists := s.financeRefunds[copy.ID]; exists {
		return false, ErrFinanceIdempotencyConflict
	}
	if copy.ProviderRefundID != "" {
		for _, existing := range s.financeRefunds {
			if existing.Provider == copy.Provider && existing.ProviderRefundID == copy.ProviderRefundID {
				return false, ErrFinanceIdempotencyConflict
			}
		}
	}
	if copy.RefundedAt.IsZero() {
		copy.RefundedAt = time.Now().UTC()
	}
	copy.CreatedAt = time.Now().UTC()
	s.financeRefunds[copy.ID] = &copy
	s.financeRefundKeys[copy.IdempotencyKey] = copy.ID
	*refund = copy
	return true, nil
}

func (s *memoryStore) ListFinanceRefunds(ctx context.Context, accountUUID string, limit int) ([]FinanceRefund, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]FinanceRefund, 0)
	for _, refund := range s.financeRefunds {
		payment := s.financePayments[refund.PaymentID]
		if payment == nil || accountUUID != "" && payment.AccountUUID != accountUUID {
			continue
		}
		result = append(result, *refund)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) BeginFinanceOperation(ctx context.Context, operation *FinanceOperation) (bool, error) {
	if operation == nil || strings.TrimSpace(operation.IdempotencyKey) == "" || strings.TrimSpace(operation.OperationType) == "" {
		return false, ErrFinanceIdempotencyConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(operation.IdempotencyKey)
	operation.OperationType = strings.TrimSpace(operation.OperationType)
	operation.TargetType = strings.TrimSpace(operation.TargetType)
	operation.TargetID = strings.TrimSpace(operation.TargetID)
	operation.Provider = financeProvider(operation.Provider)
	operation.Request = financeJSON(operation.Request)
	var refundAmount int64
	var refundCurrency string
	if operation.OperationType == "refund" {
		if operation.TargetType != "payment" || operation.TargetID == "" {
			return false, ErrFinanceIdempotencyConflict
		}
		var err error
		refundAmount, refundCurrency, err = financeRefundRequest(operation)
		if err != nil {
			return false, ErrFinanceIdempotencyConflict
		}
	}
	storedID, exists := s.financeOperationKeys[key]
	var stored FinanceOperation
	if exists {
		stored = *s.financeOperations[storedID]
		if stored.OperationType != operation.OperationType || stored.TargetType != operation.TargetType || stored.TargetID != operation.TargetID || stored.Provider != operation.Provider || !jsonEqual(stored.Request, operation.Request) {
			return false, ErrFinanceIdempotencyConflict
		}
		if stored.Status == FinanceOperationInProgress {
			return false, ErrFinanceOperationInProgress
		}
		if stored.Status == FinanceOperationSucceeded {
			*operation = stored
			return false, nil
		}
		if stored.Status != FinanceOperationPending && stored.Status != FinanceOperationFailed && stored.Status != FinanceOperationReconciliationRequired {
			return false, ErrFinanceOperationNotRetryable
		}
	} else {
		stored = *operation
		stored.ID = financeID(stored.ID)
		stored.IdempotencyKey = key
		stored.OperationType = strings.TrimSpace(stored.OperationType)
		stored.TargetType = strings.TrimSpace(stored.TargetType)
		stored.TargetID = strings.TrimSpace(stored.TargetID)
		stored.Provider = financeProvider(stored.Provider)
		stored.Status = FinanceOperationPending
		stored.Request = financeJSON(stored.Request)
		stored.Response = financeJSON(nil)
		stored.CreatedAt = time.Now().UTC()
		if _, exists := s.financeOperations[stored.ID]; exists {
			return false, ErrFinanceIdempotencyConflict
		}
	}
	if operation.OperationType == "refund" {
		payment := s.financePayments[operation.TargetID]
		if payment == nil || payment.Currency != refundCurrency {
			return false, ErrFinanceRecordNotFound
		}
		var reserved int64
		for _, refund := range s.financeRefunds {
			if refund.PaymentID == payment.ID {
				reserved += refund.AmountMinor
			}
		}
		for _, other := range s.financeOperations {
			if other.IdempotencyKey != key && financeOperationReservesRefund(other) && other.TargetID == payment.ID && s.financeRefundKeys[other.IdempotencyKey] == "" {
				amount, _, err := financeRefundRequest(other)
				if err == nil {
					reserved += amount
				}
			}
		}
		if refundAmount > payment.AmountMinor-reserved {
			return false, ErrFinanceRefundExceedsPayment
		}
	}
	if !exists {
		s.financeOperationKeys[key] = stored.ID
	}
	stored.AttemptCount++
	stored.Status = FinanceOperationInProgress
	stored.UpdatedAt = time.Now().UTC()
	s.financeOperations[stored.ID] = &stored
	operationEvent := FinanceOperationEvent{
		ID: uuid.NewString(), OperationID: stored.ID, Attempt: stored.AttemptCount,
		EventType: "attempt_started", Status: FinanceOperationInProgress,
		Payload: stored.Request, OccurredAt: stored.UpdatedAt,
	}
	s.financeOperationEvents[stored.ID] = append(s.financeOperationEvents[stored.ID], operationEvent)
	*operation = stored
	return true, nil
}

func (s *memoryStore) FinishFinanceOperation(ctx context.Context, operationID, status, providerOperationID string, response json.RawMessage, operationErr error, nextAttemptAt *time.Time) error {
	if status != FinanceOperationSucceeded && status != FinanceOperationFailed && status != FinanceOperationReconciliationRequired {
		return ErrFinanceOperationNotRetryable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	operation, ok := s.financeOperations[strings.TrimSpace(operationID)]
	if !ok {
		return ErrFinanceRecordNotFound
	}
	if operation.Status != FinanceOperationInProgress {
		return ErrFinanceOperationNotRetryable
	}
	providerOperationID = strings.TrimSpace(providerOperationID)
	if providerOperationID != "" {
		for _, existing := range s.financeOperations {
			if existing.ID != operation.ID && existing.Provider == operation.Provider && existing.OperationType == operation.OperationType && existing.ProviderOperationID == providerOperationID {
				return ErrFinanceIdempotencyConflict
			}
		}
	}
	if operationErr != nil {
		status = FinanceOperationFailed
	}
	operation.Status = status
	operation.ProviderOperationID = providerOperationID
	operation.Response = financeJSON(response)
	operation.NextAttemptAt = nextAttemptAt
	operation.LastError = ""
	if operationErr != nil {
		operation.LastError = operationErr.Error()
	}
	operation.UpdatedAt = time.Now().UTC()
	eventType := "attempt_" + status
	event := FinanceOperationEvent{ID: uuid.NewString(), OperationID: operation.ID, Attempt: operation.AttemptCount, EventType: eventType, Status: status, ProviderOperationID: operation.ProviderOperationID, Payload: financeJSON(response), Error: operation.LastError, OccurredAt: operation.UpdatedAt}
	s.financeOperationEvents[operation.ID] = append(s.financeOperationEvents[operation.ID], event)
	return nil
}

func (s *memoryStore) GetFinanceOperation(ctx context.Context, idempotencyKey string) (*FinanceOperation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.financeOperationKeys[strings.TrimSpace(idempotencyKey)]
	if !ok {
		return nil, ErrFinanceRecordNotFound
	}
	copy := *s.financeOperations[id]
	copy.Request, copy.Response = financeJSON(copy.Request), financeJSON(copy.Response)
	return &copy, nil
}

func (s *memoryStore) ListFinanceOperationsForReconciliation(ctx context.Context, limit int) ([]FinanceOperation, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]FinanceOperation, 0)
	for _, operation := range s.financeOperations {
		if operation.Status == FinanceOperationPending || operation.Status == FinanceOperationInProgress || operation.Status == FinanceOperationFailed || operation.Status == FinanceOperationReconciliationRequired || operation.OperationType == "refund" && operation.Status == FinanceOperationSucceeded && s.financeRefundKeys[operation.IdempotencyKey] == "" {
			copy := *operation
			copy.Request, copy.Response = financeJSON(copy.Request), financeJSON(copy.Response)
			result = append(result, copy)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.Before(result[j].UpdatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) ListFinanceOperationEvents(ctx context.Context, operationID string) ([]FinanceOperationEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.financeOperations[strings.TrimSpace(operationID)]; !ok {
		return nil, ErrFinanceRecordNotFound
	}
	events := append([]FinanceOperationEvent(nil), s.financeOperationEvents[strings.TrimSpace(operationID)]...)
	for i := range events {
		events[i].Payload = financeJSON(events[i].Payload)
	}
	return events, nil
}
