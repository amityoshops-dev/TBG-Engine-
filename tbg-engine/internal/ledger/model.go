package ledger

import "time"

// Account represents a ledger account: a corporate float, the internal CMS
// clearing suspense account, or the central bank Nostro settlement account.
type Account struct {
	AccountID   string
	AccountName string
	AccountType string
	Currency    string
}

// JournalVoucher is the immutable record header for a balanced set of postings.
type JournalVoucher struct {
	JVID             string
	IdempotencyKey   string
	TransactionState string
	PrevHash         string
	CurrentHash      string
	CreatedAt        time.Time
}

// Posting is one leg (debit or credit) of a journal voucher.
type Posting struct {
	PostingID int64     `json:"posting_id"`
	JVID      string    `json:"jv_id"`
	AccountID string    `json:"account_id"`
	Direction string    `json:"direction"` // "DR" or "CR"
	Amount    float64   `json:"amount"`
	CreatedAt time.Time `json:"created_at"`
}

// PayoutRequest is the inbound corporate payout instruction.
type PayoutRequest struct {
	CorporateAccount string  `json:"corporate_account"`
	Amount           float64 `json:"amount"`
	Currency         string  `json:"currency"`
	PaymentRail      string  `json:"payment_rail"`
	BeneficiaryName  string  `json:"beneficiary_name"`
	BeneficiaryAcct  string  `json:"beneficiary_acct"`
	BeneficiaryIFSC  string  `json:"beneficiary_ifsc"`
	ReferenceID      string  `json:"reference_id"`
}

// PayoutResponse is returned to the corporate ERP once a payout settles.
type PayoutResponse struct {
	Status        string            `json:"status"`
	JVID          string            `json:"jv_id"`
	UTR           string            `json:"utr"`
	MerkleHash    string            `json:"merkle_hash"`
	LedgerEntries []LedgerEntryView `json:"ledger_entries"`
	SettledAt     time.Time         `json:"settled_at"`
}

// LedgerEntryView is a simplified posting view returned in API responses.
type LedgerEntryView struct {
	AccountID string  `json:"account_id"`
	Direction string  `json:"direction"`
	Amount    float64 `json:"amount"`
}
