package wallet

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

func scanWallet(row pgx.Row) (Wallet, error) {
	var w Wallet
	err := row.Scan(&w.ID, &w.UserID, &w.Currency, &w.AvailableMinorUnits, &w.PendingMinorUnits, &w.CreatedAt, &w.UpdatedAt)
	return w, err
}
func scanTransaction(row pgx.Row) (Transaction, error) {
	var t Transaction
	err := row.Scan(&t.ID, &t.WalletID, &t.UserID, &t.Type, &t.AmountMinorUnits, &t.Currency, &t.Reference, &t.Status, &t.ActorUserID, &t.Reason, &t.CreatedAt)
	return t, err
}

func (r *Repository) GetOrCreate(ctx context.Context, userID, currency string) (Wallet, error) {
	return scanWallet(r.pool.QueryRow(ctx, `INSERT INTO wallets(user_id,currency) VALUES($1::uuid,$2) ON CONFLICT(user_id,currency) DO UPDATE SET updated_at=wallets.updated_at RETURNING id::text,user_id::text,currency,available_minor_units,pending_minor_units,created_at,updated_at`, userID, currency))
}

func (r *Repository) ListTransactions(ctx context.Context, userID, currency string, limit int) ([]Transaction, error) {
	rows, err := r.pool.Query(ctx, `SELECT wt.id::text,wt.wallet_id::text,wt.user_id::text,wt.transaction_type,wt.amount_minor_units,wt.currency,wt.reference,wt.status,wt.actor_user_id::text,wt.reason,wt.created_at FROM wallet_transactions wt WHERE wt.user_id=$1::uuid AND wt.currency=$2 ORDER BY wt.created_at DESC,wt.id DESC LIMIT $3`, userID, currency, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Transaction{}
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r *Repository) Adjust(ctx context.Context, in Adjustment) (Transaction, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Transaction{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var walletID string
	var balance int64
	err = tx.QueryRow(ctx, `INSERT INTO wallets(user_id,currency) VALUES($1::uuid,$2) ON CONFLICT(user_id,currency) DO UPDATE SET updated_at=wallets.updated_at RETURNING id::text,available_minor_units`, in.UserID, in.Currency).Scan(&walletID, &balance)
	if err != nil {
		return Transaction{}, err
	}
	delta := in.AmountMinorUnits
	if in.Type == "adjustment_debit" {
		delta = -delta
	}
	if balance+delta < 0 {
		return Transaction{}, ErrInsufficientFunds
	}
	row := tx.QueryRow(ctx, `INSERT INTO wallet_transactions(wallet_id,user_id,transaction_type,amount_minor_units,currency,reference,idempotency_key,actor_user_id,reason) VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8::uuid,$9) RETURNING id::text,wallet_id::text,user_id::text,transaction_type,amount_minor_units,currency,reference,status,actor_user_id::text,reason,created_at`, walletID, in.UserID, in.Type, in.AmountMinorUnits, in.Currency, in.Reference, in.IdempotencyKey, in.ActorUserID, in.Reason)
	result, err := scanTransaction(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Transaction{}, ErrIdempotencyConflict
		}
		return Transaction{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE wallets SET available_minor_units=available_minor_units+$2,updated_at=now() WHERE id=$1::uuid AND available_minor_units+$2>=0`, walletID, delta)
	if err != nil {
		return Transaction{}, err
	}
	if tag.RowsAffected() != 1 {
		return Transaction{}, ErrInsufficientFunds
	}
	if err := tx.Commit(ctx); err != nil {
		return Transaction{}, err
	}
	return result, nil
}

func (r *Repository) Purchase(ctx context.Context, in PurchaseDebit) (Transaction, error) {
	return r.debit(ctx, in, "purchase", "wallet-backed number purchase")
}

// Renew is the same debit as Purchase, posted under a distinct ledger type
// so recurring monthly charges are distinguishable from the original
// purchase in a customer's transaction history.
func (r *Repository) Renew(ctx context.Context, in PurchaseDebit) (Transaction, error) {
	return r.debit(ctx, in, "renewal", "wallet-backed rental renewal")
}

func (r *Repository) debit(ctx context.Context, in PurchaseDebit, transactionType, reason string) (Transaction, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Transaction{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var walletID string
	// Note: no balance pre-check here. A retried debit (same idempotency
	// key) must surface as ErrIdempotencyConflict even after the original
	// debit already reduced the balance below the requested amount, so the
	// ledger insert — which is what actually distinguishes a genuine retry
	// from a new debit — runs before any balance decision.
	err = tx.QueryRow(ctx, `INSERT INTO wallets(user_id,currency) VALUES($1::uuid,$2) ON CONFLICT(user_id,currency) DO UPDATE SET updated_at=wallets.updated_at RETURNING id::text`, in.UserID, in.Currency).Scan(&walletID)
	if err != nil {
		return Transaction{}, err
	}
	row := tx.QueryRow(ctx, `INSERT INTO wallet_transactions(wallet_id,user_id,transaction_type,amount_minor_units,currency,reference,idempotency_key,reason) VALUES($1::uuid,$2::uuid,$3,$4,$5,$6,$7,$8) RETURNING id::text,wallet_id::text,user_id::text,transaction_type,amount_minor_units,currency,reference,status,actor_user_id::text,reason,created_at`, walletID, in.UserID, transactionType, in.AmountMinorUnits, in.Currency, in.Reference, in.IdempotencyKey, reason)
	result, err := scanTransaction(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Transaction{}, ErrIdempotencyConflict
		}
		return Transaction{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE wallets SET available_minor_units=available_minor_units-$2,updated_at=now() WHERE id=$1::uuid AND available_minor_units>=$2`, walletID, in.AmountMinorUnits)
	if err != nil {
		return Transaction{}, err
	}
	if tag.RowsAffected() != 1 {
		return Transaction{}, ErrInsufficientFunds
	}
	if err := tx.Commit(ctx); err != nil {
		return Transaction{}, err
	}
	return result, nil
}

// RefundPurchase reverses a prior Purchase with a new "refund" ledger entry,
// keyed by "refund:"+the original debit's idempotency key so a retried
// compensation is a no-op (ErrIdempotencyConflict) rather than a double
// credit.
func (r *Repository) RefundPurchase(ctx context.Context, in RefundInput) (Transaction, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Transaction{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var walletID string
	err = tx.QueryRow(ctx, `INSERT INTO wallets(user_id,currency) VALUES($1::uuid,$2) ON CONFLICT(user_id,currency) DO UPDATE SET updated_at=wallets.updated_at RETURNING id::text`, in.UserID, in.Currency).Scan(&walletID)
	if err != nil {
		return Transaction{}, err
	}
	refundKey := "refund:" + in.OriginalIdempotencyKey
	row := tx.QueryRow(ctx, `INSERT INTO wallet_transactions(wallet_id,user_id,transaction_type,amount_minor_units,currency,reference,idempotency_key,reason) VALUES($1::uuid,$2::uuid,'refund',$3,$4,$5,$6,$7) RETURNING id::text,wallet_id::text,user_id::text,transaction_type,amount_minor_units,currency,reference,status,actor_user_id::text,reason,created_at`, walletID, in.UserID, in.AmountMinorUnits, in.Currency, in.Reference, refundKey, in.Reason)
	result, err := scanTransaction(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Transaction{}, ErrIdempotencyConflict
		}
		return Transaction{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE wallets SET available_minor_units=available_minor_units+$2,updated_at=now() WHERE id=$1::uuid`, walletID, in.AmountMinorUnits); err != nil {
		return Transaction{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Transaction{}, err
	}
	return result, nil
}

const fundingColumns = `id::text,user_id::text,wallet_id::text,provider_config_id::text,provider_reference,status,currency,amount_minor_units,created_at`

func scanFunding(row pgx.Row) (FundingAttempt, error) {
	var a FundingAttempt
	var ref *string
	err := row.Scan(&a.ID, &a.UserID, &a.WalletID, &a.ProviderConfigID, &ref, &a.Status, &a.Currency, &a.AmountMinorUnits, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FundingAttempt{}, ErrFundingNotFound
	}
	if ref != nil {
		a.ProviderReference = *ref
	}
	return a, err
}
func (r *Repository) CreateFundingAttempt(ctx context.Context, walletID, userID, configID string, amount int64, currency, key string) (FundingAttempt, error) {
	a, err := scanFunding(r.pool.QueryRow(ctx, `INSERT INTO wallet_funding_attempts(wallet_id,user_id,provider_config_id,amount_minor_units,currency,idempotency_key) VALUES($1::uuid,$2::uuid,$3::uuid,$4,$5,$6) RETURNING `+fundingColumns, walletID, userID, configID, amount, currency, key))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return FundingAttempt{}, ErrIdempotencyConflict
		}
	}
	return a, err
}
func (r *Repository) SetFundingReference(ctx context.Context, id, reference, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE wallet_funding_attempts SET provider_reference=$2,status=$3,updated_at=now() WHERE id=$1::uuid`, id, reference, status)
	return err
}
func (r *Repository) GetFundingAttempt(ctx context.Context, userID, id string) (FundingAttempt, error) {
	return scanFunding(r.pool.QueryRow(ctx, `SELECT `+fundingColumns+` FROM wallet_funding_attempts WHERE id=$1::uuid AND user_id=$2::uuid`, id, userID))
}
func (r *Repository) GetFundingByReference(ctx context.Context, configID, reference string) (FundingAttempt, error) {
	return scanFunding(r.pool.QueryRow(ctx, `SELECT `+fundingColumns+` FROM wallet_funding_attempts WHERE provider_config_id=$1::uuid AND provider_reference=$2`, configID, reference))
}
func (r *Repository) UpdateFundingStatus(ctx context.Context, id, status string) error {
	_, err := r.pool.Exec(ctx, `UPDATE wallet_funding_attempts SET status=$2,updated_at=now() WHERE id=$1::uuid AND status<>'success'`, id, status)
	return err
}
func (r *Repository) SettleFunding(ctx context.Context, attempt FundingAttempt, ledgerKey string) (FundingAttempt, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return FundingAttempt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	current, err := scanFunding(tx.QueryRow(ctx, `SELECT `+fundingColumns+` FROM wallet_funding_attempts WHERE id=$1::uuid FOR UPDATE`, attempt.ID))
	if err != nil {
		return FundingAttempt{}, err
	}
	if current.Status == "success" {
		if err := tx.Commit(ctx); err != nil {
			return FundingAttempt{}, err
		}
		return current, nil
	}
	if _, err = tx.Exec(ctx, `INSERT INTO wallet_transactions(wallet_id,user_id,transaction_type,amount_minor_units,currency,reference,idempotency_key,reason) VALUES($1::uuid,$2::uuid,'deposit',$3,$4,$5,$6,'Verified Paystack wallet funding') ON CONFLICT(wallet_id,idempotency_key) DO NOTHING`, current.WalletID, current.UserID, current.AmountMinorUnits, current.Currency, current.ProviderReference, ledgerKey); err != nil {
		return FundingAttempt{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE wallets SET available_minor_units=available_minor_units+$2,updated_at=now() WHERE id=$1::uuid`, current.WalletID, current.AmountMinorUnits)
	if err != nil {
		return FundingAttempt{}, err
	}
	if tag.RowsAffected() != 1 {
		return FundingAttempt{}, ErrFundingNotFound
	}
	if _, err = tx.Exec(ctx, `UPDATE wallet_funding_attempts SET status='success',updated_at=now() WHERE id=$1::uuid`, current.ID); err != nil {
		return FundingAttempt{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return FundingAttempt{}, err
	}
	current.Status = "success"
	return current, nil
}
