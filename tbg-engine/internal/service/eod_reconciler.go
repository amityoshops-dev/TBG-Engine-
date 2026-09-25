package service

import (
	"context"
	"fmt"

	"tbg-engine/internal/ledger"
	"tbg-engine/internal/observability"
)

// EODReconciler implements the midnight batch process that zeroes out the
// CMS clearing suspense account against the central bank Nostro settlement
// account, per the EOD invariant: Balance(AC_CMS_SUSPENSE_CLEARING_9999) = 0.
type EODReconciler struct {
	Repo *ledger.Repository
	Log  *observability.Logger
}

func NewEODReconciler(repo *ledger.Repository, logger *observability.Logger) *EODReconciler {
	return &EODReconciler{Repo: repo, Log: logger}
}

// RunEODBatch computes the current suspense account balance and, if
// non-zero, posts a balancing journal voucher moving the entire balance to
// the Nostro settlement account. Any inability to reconcile to exactly zero
// returns an error — in production this would raise a BOD-blocking
// exception alert rather than silently continuing to the next business day.
func (e *EODReconciler) RunEODBatch(ctx context.Context, idempotencyKey string) (float64, error) {
	balance, err := e.Repo.SuspenseBalance(ctx, SuspenseAccount)
	if err != nil {
		return 0, fmt.Errorf("fetch suspense balance: %w", err)
	}

	if balance == 0 {
		e.Log.Emit(observability.LogEntry{EventType: "EOD_NOOP", Status: "SETTLED",
			AccountID: SuspenseAccount, Amount: 0})
		return 0, nil
	}

	jvID := "EOD-" + idempotencyKey
	debitAccount, creditAccount := SuspenseAccount, NostroAccount
	amount := balance
	if balance < 0 {
		// Keep posted amounts positive while preserving the double-entry
		// invariant by swapping legs when the suspense account is net debit.
		debitAccount, creditAccount = NostroAccount, SuspenseAccount
		amount = -balance
	}

	hash, err := e.Repo.PostDoubleEntry(ctx, jvID, idempotencyKey, debitAccount, creditAccount, amount)
	if err != nil {
		e.Log.Emit(observability.LogEntry{EventType: "EOD_BALANCING_FAILED", Status: "FAILED",
			AccountID: SuspenseAccount, Amount: amount, ErrorMessage: err.Error()})
		return 0, fmt.Errorf("post EOD balancing voucher: %w", err)
	}

	finalBalance, err := e.Repo.SuspenseBalance(ctx, SuspenseAccount)
	if err != nil {
		return 0, fmt.Errorf("verify final suspense balance: %w", err)
	}
	if finalBalance != 0 {
		return finalBalance, fmt.Errorf("EOD zero-out invariant violated: suspense balance is %.4f, expected 0.0000", finalBalance)
	}

	e.Log.Emit(observability.LogEntry{EventType: "EOD_BALANCED", Status: "SETTLED",
		AccountID: SuspenseAccount, TxnID: hash, Amount: amount})

	return finalBalance, nil
}
