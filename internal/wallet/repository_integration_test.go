package wallet

import (
	"context"
	"errors"
	"testing"

	"migo/internal/testutil/dbtest"
)

func walletUsers(t *testing.T) (*Repository, string, string) {
	t.Helper()
	pool := dbtest.NewMigrated(t)
	ids := make([]string, 2)
	for i, email := range []string{"wallet-user@example.com", "wallet-admin@example.com"} {
		if err := pool.QueryRow(context.Background(), `INSERT INTO users(full_name,username,email,password_hash,role) VALUES($1,$2,$3,'hash',$4) RETURNING id::text`, `Wallet User`, "wallet-user-"+string(rune('a'+i)), email, map[bool]string{true: "admin", false: "user"}[i == 1]).Scan(&ids[i]); err != nil {
			t.Fatal(err)
		}
	}
	return NewRepository(pool), ids[0], ids[1]
}

func TestAdjustmentUpdatesBalanceAndCreatesImmutableLedger(t *testing.T) {
	repo, userID, adminID := walletUsers(t)
	ctx := context.Background()
	tx, err := repo.Adjust(ctx, Adjustment{ActorUserID: adminID, UserID: userID, Currency: "NGN", Type: "adjustment_credit", AmountMinorUnits: 2500, Reference: "manual-1", IdempotencyKey: "key-1", Reason: "support correction"})
	if err != nil {
		t.Fatal(err)
	}
	if tx.AmountMinorUnits != 2500 {
		t.Fatalf("transaction=%+v", tx)
	}
	balance, err := repo.GetOrCreate(ctx, userID, "NGN")
	if err != nil || balance.AvailableMinorUnits != 2500 {
		t.Fatalf("wallet=%+v err=%v", balance, err)
	}
	if _, err := repo.pool.Exec(ctx, `UPDATE wallet_transactions SET amount_minor_units=1 WHERE id=$1::uuid`, tx.ID); err == nil {
		t.Fatal("ledger update must be rejected")
	}
}

func TestAdjustmentRejectsOverdraftAndDuplicateKey(t *testing.T) {
	repo, userID, adminID := walletUsers(t)
	ctx := context.Background()
	base := Adjustment{ActorUserID: adminID, UserID: userID, Currency: "NGN", Type: "adjustment_credit", AmountMinorUnits: 100, Reference: "manual-1", IdempotencyKey: "key-1", Reason: "correction"}
	if _, err := repo.Adjust(ctx, base); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Adjust(ctx, base); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("duplicate error=%v", err)
	}
	base.Type = "adjustment_debit"
	base.AmountMinorUnits = 101
	base.Reference = "manual-2"
	base.IdempotencyKey = "key-2"
	if _, err := repo.Adjust(ctx, base); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overdraft error=%v", err)
	}
}

func TestFundingSettlementCreditsExactlyOnce(t *testing.T) {
	repo, userID, _ := walletUsers(t)
	ctx := context.Background()
	wallet, err := repo.GetOrCreate(ctx, userID, "NGN")
	if err != nil {
		t.Fatal(err)
	}
	var configID string
	if err := repo.pool.QueryRow(ctx, `INSERT INTO provider_configs(provider_kind,provider_key,config_name,credentials_ciphertext,credential_key_version) VALUES('payment','paystack','wallet-test',decode('01','hex'),'v1') RETURNING id::text`).Scan(&configID); err != nil {
		t.Fatal(err)
	}
	attempt, err := repo.CreateFundingAttempt(ctx, wallet.ID, userID, configID, 4500, "NGN", "fund-key")
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetFundingReference(ctx, attempt.ID, "paystack-ref", "pending"); err != nil {
		t.Fatal(err)
	}
	attempt.ProviderReference = "paystack-ref"
	attempt.Status = "pending"
	for i := 0; i < 2; i++ {
		if _, err := repo.SettleFunding(ctx, attempt, "funding:"+attempt.ID); err != nil {
			t.Fatalf("settlement %d: %v", i, err)
		}
	}
	balance, err := repo.GetOrCreate(ctx, userID, "NGN")
	if err != nil || balance.AvailableMinorUnits != 4500 {
		t.Fatalf("wallet=%+v err=%v", balance, err)
	}
	var count int
	if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM wallet_transactions WHERE wallet_id=$1::uuid AND transaction_type='deposit'`, wallet.ID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("deposit count=%d err=%v", count, err)
	}
}
