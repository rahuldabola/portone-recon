// Package model holds the shapes shared by ingest, reconciliation and reporting.
package model

import (
	"time"

	"github.com/shopspring/decimal"
)

// Source identifies which of the two Amazon reports a ledger row came from.
type Source string

const (
	SourcePayment    Source = "PAYMENT"
	SourceSettlement Source = "SETTLEMENT"
)

// LedgerEntry is one amount component from either report.
//
// A settlement row is already a single amount component. A payments row spreads
// its money across typed columns, so it fans out into one LedgerEntry per
// populated column — which is exactly the grain the payment config addresses
// through `amount_field`. Both sources therefore share one table and one shape.
type LedgerEntry struct {
	// provenance
	Source     Source
	SourceFile string
	SourceLine int
	RawPayload map[string]string

	// identity
	SettlementID    string
	TxnRef          string
	MerchantOrderID string
	AdjustmentID    string
	ShipmentID      string
	SKU             string
	Quantity        *int

	// classification (normalised)
	TransactionType   string
	Description       string
	AmountType        string
	AmountDescription string
	AmountField       string

	// raw passthroughs for the audit sheet
	TransactionTypeRaw string
	DescriptionRaw     string
	Marketplace        string
	Fulfilment         string
	OrderCity          string
	OrderState         string
	OrderPostal        string
	TransactionStatus  string

	// money & time
	Amount     decimal.Decimal
	PostedAt   time.Time
	ReleasedAt time.Time
	EventDate  time.Time

	// config resolution
	RecordRef    string
	SummaryField string
	ConfigID     *int
	MatchKind    string
	RouteSeq     int
	InScope      bool
}

// Settlement describes the settlement-level header row of the settlement file.
type Settlement struct {
	ID          string
	StartDate   time.Time
	EndDate     time.Time
	DepositDate time.Time
	TotalAmount decimal.Decimal
	Currency    string
}
