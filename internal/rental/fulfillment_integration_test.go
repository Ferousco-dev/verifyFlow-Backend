package rental

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRepositoryGetFulfillmentSnapshotReflectsReservationAndActivation(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	numberID := f.addNumber(t, "snapshot")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "snapshot-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	orderID := reservation.Order.ID

	snapshot, err := f.repository.GetFulfillmentSnapshot(context.Background(), orderID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.OrderID != orderID || snapshot.ProviderNumberID != numberID || snapshot.ProviderConfigID != f.configID ||
		snapshot.NumberType != "Local" || !snapshot.SMSEnabled || snapshot.DurationSeconds != 3600 || snapshot.ActivatedAt != nil {
		t.Fatalf("snapshot before activation = %+v", snapshot)
	}

	if err := f.repository.ActivateRental(context.Background(), orderID, numberID, "PNtest123", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	activated, err := f.repository.GetFulfillmentSnapshot(context.Background(), orderID)
	if err != nil || activated.ActivatedAt == nil {
		t.Fatalf("snapshot after activation = %+v, %v", activated, err)
	}

	var numberStatus, providerReference string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT status, provider_reference FROM provider_numbers WHERE id = $1::uuid`, numberID,
	).Scan(&numberStatus, &providerReference); err != nil {
		t.Fatal(err)
	}
	if numberStatus != "ACTIVE" || providerReference != "PNtest123" {
		t.Fatalf("number after activation: status=%s reference=%s", numberStatus, providerReference)
	}
}

func TestRepositoryGetFulfillmentSnapshotUnknownOrderReturnsNotFound(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	if _, err := f.repository.GetFulfillmentSnapshot(context.Background(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
}

func TestRepositoryActivateRentalRejectsDoubleActivation(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	numberID := f.addNumber(t, "double-activate")
	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: numberID, RentalPlanID: f.planID, IdempotencyKey: "double-activate-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	orderID := reservation.Order.ID

	if err := f.repository.ActivateRental(context.Background(), orderID, numberID, "PNtest123", time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.repository.ActivateRental(context.Background(), orderID, numberID, "PNtest456", time.Now().Add(time.Hour), time.Now()); !errors.Is(err, ErrAlreadyActivated) {
		t.Fatalf("second activation error = %v", err)
	}
}

func TestRepositorySwapToReplacementNumberFindsMatchingCapabilities(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	failedNumberID := f.addNumber(t, "swap-failed")
	replacementID := f.addNumberWithCapabilities(t, "swap-replacement", "Local", true, false, false)
	// A wrong-type available number must never be picked.
	f.addNumberWithCapabilities(t, "swap-wrong-type", "TollFree", true, false, false)

	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: failedNumberID, RentalPlanID: f.planID, IdempotencyKey: "swap-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	orderID := reservation.Order.ID

	newID, found, err := f.repository.SwapToReplacementNumber(context.Background(), orderID, failedNumberID, "Local", true, false, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !found || newID != replacementID {
		t.Fatalf("swap result: found=%v newID=%s want=%s", found, newID, replacementID)
	}

	var failedStatus, replacementStatus, rentalNumberID string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM provider_numbers WHERE id = $1::uuid`, failedNumberID).Scan(&failedStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM provider_numbers WHERE id = $1::uuid`, replacementID).Scan(&replacementStatus); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(context.Background(), `SELECT provider_number_id::text FROM rentals WHERE order_id = $1::uuid`, orderID).Scan(&rentalNumberID); err != nil {
		t.Fatal(err)
	}
	if failedStatus != "AVAILABLE" || replacementStatus != "RESERVED" || rentalNumberID != replacementID {
		t.Fatalf("post-swap state: failed=%s replacement=%s rental->%s", failedStatus, replacementStatus, rentalNumberID)
	}

	order, err := f.repository.GetOrder(context.Background(), f.userID, orderID)
	if err != nil || order.Status != "PENDING" {
		t.Fatalf("order status after successful swap = %+v, %v", order, err)
	}
}

func TestRepositorySwapToReplacementNumberMarksPendingFulfillmentWhenNoneAvailable(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	failedNumberID := f.addNumber(t, "swap-none-available")

	reservation, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: failedNumberID, RentalPlanID: f.planID, IdempotencyKey: "swap-none-key",
	})
	if err != nil {
		t.Fatal(err)
	}
	orderID := reservation.Order.ID

	newID, found, err := f.repository.SwapToReplacementNumber(context.Background(), orderID, failedNumberID, "Local", true, false, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if found || newID != "" {
		t.Fatalf("expected no replacement, got found=%v newID=%s", found, newID)
	}

	var failedStatus string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM provider_numbers WHERE id = $1::uuid`, failedNumberID).Scan(&failedStatus); err != nil {
		t.Fatal(err)
	}
	if failedStatus != "RESERVED" {
		t.Fatalf("failed number must stay RESERVED for operator attention, got %s", failedStatus)
	}

	order, err := f.repository.GetOrder(context.Background(), f.userID, orderID)
	if err != nil || order.Status != "PENDING_FULFILLMENT" {
		t.Fatalf("order status = %+v, %v", order, err)
	}
}
