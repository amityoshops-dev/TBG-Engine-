package ledger

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// Repository wraps all PostgreSQL access for the double-entry ledger.
type Repository struct {
	DB *sql.DB
}

func NewRepository(db *sql.DB) *Repository {
	return &Repository{DB: db}
}

// PostDoubleEntry commits a balanced journal voucher (one DR leg, one CR leg)
// inside a single atomic PostgreSQL transaction. The verify_jv_balance()
// trigger enforces sum(DR) - sum(CR) = 0 at the database level; this function
// never issues UPDATE/DELETE against journal_vouchers or postings, preserving
// the append-only invariant.
func (r *Repository) PostDoubleEntry(ctx context.Context, jvID, idempotencyKey, debitAccount, creditAccount string, amount float64) (string, error) {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	prevHash, err := r.lastHash(ctx, tx)
	if err != nil {
		return "", fmt.Errorf("fetch last hash: %w", err)
	}

	currentHash := computeHash(prevHash, jvID, idempotencyKey, amount)

	_, err = tx.ExecContext(ctx, `
		INSERT INTO journal_vouchers (jv_id, idempotency_key, transaction_state, prev_hash, current_hash)
		VALUES ($1, $2, 'POSTED_INTERNAL', $3, $4)`,
		jvID, idempotencyKey, prevHash, currentHash)
	if err != nil {
		return "", fmt.Errorf("insert voucher: %w", err)
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO postings (jv_id, account_id, direction, amount)
		VALUES ($1, $2, 'DR', $3), ($1, $4, 'CR', $3)`,
		jvID, debitAccount, amount, creditAccount)
	if err != nil {
		return "", fmt.Errorf("insert postings (double-entry invariant likely violated): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit tx: %w", err)
	}

	return currentHash, nil
}

func (r *Repository) lastHash(ctx context.Context, tx *sql.Tx) (string, error) {
	var hash string
	err := tx.QueryRowContext(ctx, `SELECT current_hash FROM journal_vouchers ORDER BY created_at DESC LIMIT 1`).Scan(&hash)
	if err == sql.ErrNoRows {
		return "GENESIS0000000000000000000000000000000000000000000000000000", nil
	}
	if err != nil {
		return "", err
	}
	return hash, nil
}

func computeHash(prevHash, jvID, idempotencyKey string, amount float64) string {
	raw := fmt.Sprintf("%s|%s|%s|%f", prevHash, jvID, idempotencyKey, amount)
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// RecentPostings returns the most recent N postings, newest first, for
// dashboard/audit views.
func (r *Repository) RecentPostings(ctx context.Context, limit int) ([]Posting, error) {
	rows, err := r.DB.QueryContext(ctx, `
		SELECT posting_id, jv_id, account_id, direction, amount, created_at
		FROM postings ORDER BY posting_id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Posting
	for rows.Next() {
		var p Posting
		if err := rows.Scan(&p.PostingID, &p.JVID, &p.AccountID, &p.Direction, &p.Amount, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SuspenseBalance computes the current net balance of a given account:
// CR increases the balance, DR decreases it (standard convention for a
// clearing/suspense liability account).
func (r *Repository) SuspenseBalance(ctx context.Context, accountID string) (float64, error) {
	var balance float64
	err := r.DB.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN direction = 'CR' THEN amount ELSE -amount END), 0)
		FROM postings WHERE account_id = $1`, accountID).Scan(&balance)
	return balance, err
}
