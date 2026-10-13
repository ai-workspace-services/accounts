package store

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

func (s *memoryStore) EnsureReferralCode(ctx context.Context, userUUID, candidate string) (*ReferralCode, error) {
	_ = ctx
	userUUID = strings.TrimSpace(userUUID)
	candidate = strings.ToUpper(strings.TrimSpace(candidate))
	s.mu.Lock()
	defer s.mu.Unlock()
	if old := s.referralCodes[userUUID]; old != nil {
		copy := *old
		return &copy, nil
	}
	if s.byID[userUUID] == nil || candidate == "" {
		return nil, ErrUserNotFound
	}
	if owner := s.referralCodesByValue[candidate]; owner != "" && owner != userUUID {
		return nil, ErrReferralAlreadyAttributed
	}
	code := &ReferralCode{UserUUID: userUUID, Code: candidate, CreatedAt: time.Now().UTC()}
	s.referralCodes[userUUID] = code
	s.referralCodesByValue[candidate] = userUUID
	copy := *code
	return &copy, nil
}

func (s *memoryStore) GetReferralCode(ctx context.Context, code string) (*ReferralCode, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	owner := s.referralCodesByValue[strings.ToUpper(strings.TrimSpace(code))]
	if owner == "" {
		return nil, ErrReferralNotFound
	}
	copy := *s.referralCodes[owner]
	return &copy, nil
}

func (s *memoryStore) CreateReferralAttribution(ctx context.Context, code, inviteeUUID string, createdAt time.Time) error {
	_ = ctx
	code, inviteeUUID = strings.ToUpper(strings.TrimSpace(code)), strings.TrimSpace(inviteeUUID)
	s.mu.Lock()
	defer s.mu.Unlock()
	referrerUUID := s.referralCodesByValue[code]
	if referrerUUID == "" || s.byID[inviteeUUID] == nil {
		return ErrReferralNotFound
	}
	if referrerUUID == inviteeUUID {
		return ErrReferralSelf
	}
	if _, exists := s.referralsByInvitee[inviteeUUID]; exists {
		return ErrReferralAlreadyAttributed
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	s.referralsByInvitee[inviteeUUID] = &ReferralAttribution{ID: uuid.NewString(), ReferrerUUID: referrerUUID, InviteeUUID: inviteeUUID, Code: code, CreatedAt: createdAt.UTC()}
	return nil
}

func (s *memoryStore) GetReferralReferrer(ctx context.Context, inviteeUUID string) (string, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	referral := s.referralsByInvitee[strings.TrimSpace(inviteeUUID)]
	if referral == nil {
		return "", ErrReferralNotFound
	}
	return referral.ReferrerUUID, nil
}

func (s *memoryStore) CreateReferralReward(ctx context.Context, reward *ReferralReward) (bool, error) {
	_ = ctx
	if reward == nil || reward.ProviderInvoiceID == "" || reward.PaidMinor <= 0 || reward.RewardMinor != reward.PaidMinor/10 || reward.Currency == "" || reward.ProductSlug != "xconnect" || (reward.PlanID != "PRO-MONTHLY" && reward.PlanID != "PRO-YEARLY") {
		return false, ErrReferralNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior := s.referralRewardByInvoice[reward.ProviderInvoiceID]; prior != "" {
		reward.ID = prior
		return false, nil
	}
	referral := s.referralsByInvitee[reward.InviteeUUID]
	if referral == nil || (reward.ReferrerUUID != "" && referral.ReferrerUUID != reward.ReferrerUUID) {
		return false, ErrReferralNotFound
	}
	reward.ReferrerUUID = referral.ReferrerUUID
	for _, old := range s.referralRewards {
		if old.InviteeUUID == reward.InviteeUUID {
			return false, nil
		}
	}
	for _, risk := range s.referralRisks {
		if risk.ProviderInvoiceID == reward.ProviderInvoiceID {
			return false, nil
		}
	}
	copy := *reward
	copy.ID = uuid.NewString()
	copy.ReferralID = referral.ID
	copy.ReferralCreatedAt = referral.CreatedAt
	if copy.PaidAt.IsZero() {
		copy.PaidAt = time.Now().UTC()
	}
	copy.EligibleAt = copy.PaidAt.Add(7 * 24 * time.Hour)
	if copy.Status == "" {
		copy.Status = "pending"
	}
	copy.CreatedAt = time.Now().UTC()
	s.referralRewards[copy.ID] = &copy
	s.referralRewardByInvoice[copy.ProviderInvoiceID] = copy.ID
	*reward = copy
	return true, nil
}

func (s *memoryStore) RecordReferralRisk(ctx context.Context, risk ReferralPaymentRisk) error {
	_ = ctx
	if strings.TrimSpace(risk.SourceKey) == "" || strings.TrimSpace(risk.ProviderInvoiceID) == "" {
		return errors.New("referral risk source and invoice are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.referralRisks[risk.SourceKey]; ok && old.ProviderInvoiceID != risk.ProviderInvoiceID {
		return ErrFinanceIdempotencyConflict
	}
	s.referralRisks[risk.SourceKey] = risk
	if id := s.referralRewardByInvoice[risk.ProviderInvoiceID]; id != "" {
		s.referralRewards[id].Status = "reversed"
		for _, settlement := range s.referralSettlements {
			if settlement.Status != "pending_review" {
				continue
			}
			for _, reward := range s.referralRewards {
				if reward.ID == id && reward.ReferrerUUID == settlement.ReferrerUUID && reward.Currency == settlement.Currency {
					settlement.Reason = "payment risk hold: " + risk.Type
					break
				}
			}
		}
	}
	return nil
}

func (s *memoryStore) ListReferralRewards(ctx context.Context, referrerUUID string, limit int) ([]ReferralReward, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []ReferralReward
	for _, reward := range s.referralRewards {
		if reward.ReferrerUUID != referrerUUID {
			continue
		}
		copy := *reward
		if invitee := s.byID[reward.InviteeUUID]; invitee != nil {
			copy.InviteeEmail = invitee.Email
		}
		result = append(result, copy)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) GetReferralSummary(ctx context.Context, referrerUUID string, now time.Time) (*ReferralSummary, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := &ReferralSummary{}
	byCurrency := map[string]*ReferralCurrencySummary{}
	get := func(currency string) *ReferralCurrencySummary {
		if byCurrency[currency] == nil {
			byCurrency[currency] = &ReferralCurrencySummary{Currency: currency}
		}
		return byCurrency[currency]
	}
	for _, reward := range s.referralRewards {
		if reward.ReferrerUUID != referrerUUID {
			continue
		}
		row := get(reward.Currency)
		if reward.Status == "reversed" {
			row.ReversedMinor += reward.RewardMinor
			continue
		}
		result.SuccessfulInvites++
		row.EarnedMinor += reward.RewardMinor
		if reward.Status == "pending" && reward.EligibleAt.After(now) {
			row.PendingMinor += reward.RewardMinor
		} else if reward.Status == "pending" || reward.Status == "eligible" {
			row.EligibleMinor += reward.RewardMinor
		}
	}
	for _, ext := range s.referralExtensions {
		if ext.UserUUID == referrerUUID && ext.Status == "applied" {
			result.AwardedDays += ext.Days
		}
	}
	for _, settlement := range s.referralSettlements {
		if settlement.ReferrerUUID != referrerUUID {
			continue
		}
		row := get(settlement.Currency)
		if settlement.Status == "paid" {
			row.SettledMinor += settlement.AmountMinor
		}
		if settlement.Status == "pending_review" {
			row.EligibleMinor -= settlement.AmountMinor
		}
	}
	for _, row := range byCurrency {
		result.CashByCurrency = append(result.CashByCurrency, *row)
	}
	return result, nil
}

func (s *memoryStore) ListReferralSettlements(ctx context.Context, referrerUUID string, limit int) ([]ReferralSettlement, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []ReferralSettlement
	for _, item := range s.referralSettlements {
		if item.ReferrerUUID == referrerUUID {
			result = append(result, *item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].RequestedAt.After(result[j].RequestedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) RequestReferralSettlement(ctx context.Context, referrerUUID, currency string, now time.Time) (*ReferralSettlement, error) {
	_ = ctx
	currency = strings.ToUpper(strings.TrimSpace(currency))
	s.mu.Lock()
	defer s.mu.Unlock()
	var amount int64
	for _, reward := range s.referralRewards {
		if reward.ReferrerUUID != referrerUUID || reward.Currency != currency || reward.Status == "reversed" || reward.EligibleAt.After(now) {
			continue
		}
		if _, risky := s.referralRisks[reward.ProviderInvoiceID]; risky {
			continue
		}
		reserved := false
		for _, settlement := range s.referralSettlements {
			if settlement.ReferrerUUID == referrerUUID && settlement.Currency == currency && (settlement.Status == "pending_review" || settlement.Status == "paid") {
				reserved = true
				break
			}
		}
		if !reserved {
			amount += reward.RewardMinor
		}
	}
	if amount <= 0 {
		return nil, ErrReferralSettlementUnavailable
	}
	item := &ReferralSettlement{ID: uuid.NewString(), ReferrerUUID: referrerUUID, Currency: currency, AmountMinor: amount, Status: "pending_review", RequestedAt: now.UTC()}
	s.referralSettlements[item.ID] = item
	copy := *item
	return &copy, nil
}

func (s *memoryStore) CompleteReferralSettlement(ctx context.Context, settlementID, actorUUID, payoutReference string, now time.Time) (*ReferralSettlement, error) {
	_ = ctx
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.referralSettlements[strings.TrimSpace(settlementID)]
	if item == nil || strings.TrimSpace(payoutReference) == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	if item.Status == "paid" && item.PayoutReference == strings.TrimSpace(payoutReference) {
		copy := *item
		return &copy, nil
	}
	if item.Status != "pending_review" {
		return nil, ErrReferralSettlementUnavailable
	}
	for _, reward := range s.referralRewards {
		if reward.ReferrerUUID == item.ReferrerUUID && reward.Currency == item.Currency && reward.EligibleAt.After(now) {
			continue
		}
		if reward.ReferrerUUID == item.ReferrerUUID && reward.Currency == item.Currency && reward.Status == "reversed" {
			return nil, ErrReferralSettlementRisk
		}
	}
	item.Status = "paid"
	item.PayoutReference = strings.TrimSpace(payoutReference)
	item.CompletedAt = &now
	copy := *item
	return &copy, nil
}

func (s *memoryStore) ReconcileReferralSettlement(ctx context.Context, settlementID, actorUUID, payoutReference, reason string, now time.Time) (*ReferralSettlement, error) {
	_ = ctx
	_ = actorUUID
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.referralSettlements[strings.TrimSpace(settlementID)]
	if item == nil || strings.TrimSpace(payoutReference) == "" || strings.TrimSpace(reason) == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	if item.Status == "paid" && item.PayoutReference == strings.TrimSpace(payoutReference) {
		copy := *item
		return &copy, nil
	}
	if item.Status != "pending_review" && item.Status != "rejected" {
		return nil, ErrReferralSettlementUnavailable
	}
	if item.Status == "pending_review" {
		hasRisk := false
		for _, reward := range s.referralRewards {
			if reward.ReferrerUUID == item.ReferrerUUID && reward.Currency == item.Currency && reward.Status == "reversed" {
				hasRisk = true
				break
			}
		}
		if !hasRisk {
			return nil, ErrReferralSettlementUnavailable
		}
	}
	item.Status = "paid"
	item.PayoutReference = strings.TrimSpace(payoutReference)
	item.Reason = strings.TrimSpace(item.Reason + "; reconciled actual payment: " + reason)
	item.CompletedAt = &now
	copy := *item
	return &copy, nil
}

func (s *memoryStore) RejectReferralSettlement(ctx context.Context, settlementID, actorUUID, reason string, now time.Time) (*ReferralSettlement, error) {
	_ = ctx
	_ = actorUUID
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.referralSettlements[strings.TrimSpace(settlementID)]
	if item == nil || item.Status != "pending_review" || item.PayoutReference != "" || strings.TrimSpace(reason) == "" {
		return nil, ErrReferralSettlementUnavailable
	}
	item.Status = "rejected"
	item.Reason = strings.TrimSpace(reason)
	item.CompletedAt = &now
	copy := *item
	return &copy, nil
}

func (s *memoryStore) ReverseReferralReward(ctx context.Context, rewardID, actorUUID, sourceKey, reason string, now time.Time) error {
	_ = ctx
	_ = actorUUID
	_ = reason
	_ = now
	s.mu.Lock()
	defer s.mu.Unlock()
	if id := s.referralRewardByInvoice["source:"+sourceKey]; id != "" {
		return nil
	}
	reward := s.referralRewards[strings.TrimSpace(rewardID)]
	if reward == nil {
		return ErrReferralNotFound
	}
	if reward.Status == "reversed" {
		return nil
	}
	reward.Status = "reversed"
	return nil
}

func (s *memoryStore) ListSubscriptionExtensions(ctx context.Context, userUUID string, limit int) ([]SubscriptionExtension, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	var result []SubscriptionExtension
	for _, item := range s.referralExtensions {
		if item.UserUUID == userUUID {
			result = append(result, *item)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.After(result[j].CreatedAt) })
	if limit > 0 && len(result) > limit {
		result = result[:limit]
	}
	return result, nil
}

func (s *memoryStore) ReferralExtensionEnd(ctx context.Context, subscriptionUUID string) (time.Time, error) {
	_ = ctx
	s.mu.RLock()
	defer s.mu.RUnlock()
	var base time.Time
	days := 0
	for _, invoice := range s.financeInvoices {
		if invoice.SubscriptionUUID != subscriptionUUID || invoice.Provider != "stripe" || invoice.ServicePeriodStart == nil || invoice.ServicePeriodEnd == nil || !invoice.ServicePeriodStart.Before(*invoice.ServicePeriodEnd) || (invoice.Description != "Verified XConnect subscription invoice" && invoice.Description != "Verified XConnect inviter subscription invoice") {
			continue
		}
		paid := false
		for _, payment := range s.financePayments {
			if payment.InvoiceID != invoice.ID || payment.Provider != "stripe" {
				continue
			}
			refunded := false
			for _, refund := range s.financeRefunds {
				if refund.PaymentID == payment.ID {
					refunded = true
					break
				}
			}
			atRisk := false
			for _, risk := range s.referralRisks {
				if risk.ProviderInvoiceID == invoice.ProviderInvoiceID {
					atRisk = true
					break
				}
			}
			if !refunded && !atRisk {
				paid = true
				break
			}
		}
		if paid && (base.IsZero() || invoice.ServicePeriodEnd.After(base)) {
			base = *invoice.ServicePeriodEnd
		}
	}
	for _, item := range s.referralExtensions {
		if item.SubscriptionUUID == subscriptionUUID {
			if item.Status == "applied" {
				days += item.Days
			}
		}
	}
	if base.IsZero() {
		return time.Time{}, nil
	}
	return base.Add(time.Duration(days) * 24 * time.Hour), nil
}
