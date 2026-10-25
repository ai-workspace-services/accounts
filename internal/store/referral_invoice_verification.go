package store

import (
	"context"
	"strings"
)

// ReferralInvoiceVerification records immutable provider-price evidence without
// rewriting a previously settled invoice's description or issuance timestamp.
type ReferralInvoiceVerification struct{ InvoiceID, ProviderPriceID, PlanID, ProductSlug string }
type ReferralInvoiceVerificationStore interface {
	VerifyReferralInvoice(context.Context, *ReferralInvoiceVerification) error
}

func validReferralInvoiceVerification(v *ReferralInvoiceVerification) bool {
	return v != nil && strings.TrimSpace(v.InvoiceID) != "" && strings.TrimSpace(v.ProviderPriceID) != "" && v.ProductSlug == "xconnect" && isXConnectReferralPlan(v.PlanID)
}
func (s *memoryStore) VerifyReferralInvoice(_ context.Context, v *ReferralInvoiceVerification) error {
	if !validReferralInvoiceVerification(v) {
		return ErrReferralNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.financeInvoices[v.InvoiceID] == nil {
		return ErrReferralNotFound
	}
	if old, ok := s.referralInvoiceProofs[v.InvoiceID]; ok {
		if old != *v {
			return ErrFinanceIdempotencyConflict
		}
		return nil
	}
	s.referralInvoiceProofs[v.InvoiceID] = *v
	return nil
}
func (s *postgresStore) VerifyReferralInvoice(ctx context.Context, v *ReferralInvoiceVerification) error {
	if !validReferralInvoiceVerification(v) {
		return ErrReferralNotFound
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO public.referral_verified_invoices(invoice_id,provider_price_id,plan_id,product_slug) VALUES($1,$2,$3,$4) ON CONFLICT(invoice_id) DO NOTHING`, v.InvoiceID, v.ProviderPriceID, v.PlanID, v.ProductSlug)
	if err != nil {
		return err
	}
	var old ReferralInvoiceVerification
	err = s.db.QueryRowContext(ctx, `SELECT invoice_id::text,provider_price_id,plan_id,product_slug FROM public.referral_verified_invoices WHERE invoice_id=$1`, v.InvoiceID).Scan(&old.InvoiceID, &old.ProviderPriceID, &old.PlanID, &old.ProductSlug)
	if err != nil {
		return err
	}
	if old != *v {
		return ErrFinanceIdempotencyConflict
	}
	return nil
}
