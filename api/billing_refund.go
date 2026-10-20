package api

import "time"

const refundEligibilityWindow = 72 * time.Hour

type subscriptionRefundRequest struct {
	ExternalID string `json:"externalId"`
}
