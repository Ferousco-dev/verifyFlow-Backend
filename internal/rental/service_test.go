package rental

import (
	"context"
	"errors"
	"testing"
	"time"
)

type fakeStore struct {
	reserveInput ReserveInput
	reservation  Reservation
	reserveErr   error
	order        Order
	getErr       error
	expired      bool
	expireErr    error
	numbersPage  NumbersPage
	searchErr    error
	plans        []Plan
	plansErr     error
}

func (f *fakeStore) SearchNumbers(context.Context, NumberFilter) (NumbersPage, error) {
	return f.numbersPage, f.searchErr
}

func (f *fakeStore) ListPlans(context.Context) ([]Plan, error) {
	return f.plans, f.plansErr
}

func (f *fakeStore) Reserve(_ context.Context, input ReserveInput) (Reservation, error) {
	f.reserveInput = input
	return f.reservation, f.reserveErr
}

func (f *fakeStore) GetOrder(context.Context, string, string) (Order, error) {
	return f.order, f.getErr
}

func (f *fakeStore) ExpireReservation(context.Context, string, time.Time) (bool, error) {
	return f.expired, f.expireErr
}

func TestServiceReserveUsesTrustedIdentityAndServerDeadline(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }

	request := ReserveRequest{ProviderNumberID: "number-uuid", RentalPlanID: "plan-uuid", IdempotencyKey: "checkout-123"}
	if _, err := service.Reserve(context.Background(), "authenticated-user-uuid", request); err != nil {
		t.Fatal(err)
	}
	got := store.reserveInput
	if got.UserID != "authenticated-user-uuid" || got.ProviderNumberID != request.ProviderNumberID ||
		got.RentalPlanID != request.RentalPlanID || got.IdempotencyKey != request.IdempotencyKey {
		t.Fatalf("store input did not preserve trusted identity/request identifiers: %+v", got)
	}
	if !got.ReservedAt.Equal(now) || !got.ReservationExpiresAt.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("reservation timestamps = %s/%s", got.ReservedAt, got.ReservationExpiresAt)
	}
}

func TestServiceReserveRejectsInvalidInputBeforeStore(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		userID  string
		request ReserveRequest
	}{
		{name: "missing authenticated identity", request: ReserveRequest{ProviderNumberID: "number", RentalPlanID: "plan", IdempotencyKey: "key"}},
		{name: "missing number", userID: "user", request: ReserveRequest{RentalPlanID: "plan", IdempotencyKey: "key"}},
		{name: "missing plan", userID: "user", request: ReserveRequest{ProviderNumberID: "number", IdempotencyKey: "key"}},
		{name: "missing idempotency key", userID: "user", request: ReserveRequest{ProviderNumberID: "number", RentalPlanID: "plan"}},
		{name: "oversized idempotency key", userID: "user", request: ReserveRequest{ProviderNumberID: "number", RentalPlanID: "plan", IdempotencyKey: string(make([]byte, 201))}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := service.Reserve(context.Background(), testCase.userID, testCase.request); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Reserve error = %v", err)
			}
		})
	}
	if store.reserveInput.UserID != "" {
		t.Fatal("invalid inputs must not reach the store")
	}
}

func TestNewServiceRequiresStoreAndPositiveTTL(t *testing.T) {
	if _, err := NewService(nil, time.Minute); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil store error = %v", err)
	}
	if _, err := NewService(&fakeStore{}, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("zero TTL error = %v", err)
	}
}

func TestServiceSearchNumbersRejectsInvalidType(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SearchNumbers(context.Background(), NumberFilter{NumberType: "bogus"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("SearchNumbers error = %v", err)
	}
}

func TestServiceSearchNumbersClampsLimit(t *testing.T) {
	store := &fakeStore{}
	service, err := NewService(store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.SearchNumbers(context.Background(), NumberFilter{Limit: 1000}); err != nil {
		t.Fatal(err)
	}
}

func TestServicePassesAuthenticatedIdentityForOrderLookup(t *testing.T) {
	want := Order{ID: "order-uuid", UserID: "user-uuid", Status: "PENDING", PriceMinorUnits: 1200, Currency: "USD"}
	store := &fakeStore{order: want}
	service, err := NewService(store, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.GetOrder(context.Background(), "user-uuid", "order-uuid")
	if err != nil || got.ID != want.ID || got.UserID != want.UserID {
		t.Fatalf("GetOrder = %+v, %v", got, err)
	}
}
