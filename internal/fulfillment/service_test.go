package fulfillment

import (
	"context"
	"errors"
	"testing"
	"time"

	"migo/internal/rental"
	"migo/internal/telephony"
)

// ---- fakes ----

type fakeRentals struct {
	snapshot      rental.FulfillmentSnapshot
	snapshotAfter rental.FulfillmentSnapshot // returned on the 2nd GetFulfillmentSnapshot call, if set
	snapshotCalls int
	swapNewID     string
	swapFound     bool
	swapErr       error
	activateErr   error
	activatedWith [4]string // orderID, providerNumberID, providerReference, duration(as string)
}

func (f *fakeRentals) GetFulfillmentSnapshot(_ context.Context, orderID string) (rental.FulfillmentSnapshot, error) {
	f.snapshotCalls++
	if f.snapshotCalls > 1 && f.snapshotAfter.OrderID != "" {
		return f.snapshotAfter, nil
	}
	return f.snapshot, nil
}

func (f *fakeRentals) SwapToReplacementNumber(_ context.Context, orderID, failedNumberID, numberType string, sms, mms, voice bool) (string, bool, error) {
	return f.swapNewID, f.swapFound, f.swapErr
}

func (f *fakeRentals) ActivateRental(_ context.Context, orderID, providerNumberID, providerReference string, durationSeconds int32) error {
	f.activatedWith = [4]string{orderID, providerNumberID, providerReference, ""}
	return f.activateErr
}

type fakeProvider struct {
	provisionErr  error
	provisionedTo string
}

func (f *fakeProvider) SearchNumbers(context.Context, telephony.SearchNumbersRequest) (telephony.NumberSearchPage, error) {
	return telephony.NumberSearchPage{}, nil
}

func (f *fakeProvider) ProvisionNumber(_ context.Context, req telephony.ProvisionNumberRequest) (telephony.Number, error) {
	if f.provisionErr != nil {
		return telephony.Number{}, f.provisionErr
	}
	f.provisionedTo = req.PhoneNumber
	return telephony.Number{ProviderReference: "PN123", PhoneNumber: req.PhoneNumber}, nil
}

func (f *fakeProvider) ReleaseNumber(context.Context, string) error { return nil }

func (f *fakeProvider) SendMessage(context.Context, telephony.SendMessageRequest) (telephony.MessageReceipt, error) {
	return telephony.MessageReceipt{}, nil
}

type fakeResolver struct {
	provider telephony.Provider
	err      error
}

func (f fakeResolver) Resolve(context.Context, string) (telephony.Provider, error) {
	return f.provider, f.err
}

// ---- tests ----

func TestFulfillProvisionsAndActivatesOnFirstTry(t *testing.T) {
	rentals := &fakeRentals{snapshot: rental.FulfillmentSnapshot{
		OrderID: "order-1", ProviderNumberID: "number-1", PhoneNumber: "+14155550001",
		NumberType: "Local", SMSEnabled: true, DurationSeconds: 3600,
	}}
	provider := &fakeProvider{}
	svc, err := NewService(rentals, fakeResolver{provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Fulfill(context.Background(), "order-1"); err != nil {
		t.Fatal(err)
	}
	if provider.provisionedTo != "+14155550001" {
		t.Fatalf("provisioned = %q", provider.provisionedTo)
	}
	if rentals.activatedWith[0] != "order-1" || rentals.activatedWith[1] != "number-1" || rentals.activatedWith[2] != "PN123" {
		t.Fatalf("activated with = %+v", rentals.activatedWith)
	}
}

func TestFulfillIsIdempotentWhenAlreadyActivated(t *testing.T) {
	activatedAt := time.Now()
	rentals := &fakeRentals{snapshot: rental.FulfillmentSnapshot{OrderID: "order-1", ActivatedAt: &activatedAt}}
	provider := &fakeProvider{provisionErr: errors.New("must not be called")}
	svc, err := NewService(rentals, fakeResolver{provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Fulfill(context.Background(), "order-1"); err != nil {
		t.Fatal(err)
	}
}

func TestFulfillSwapsToReplacementWhenFirstProvisionFails(t *testing.T) {
	rentals := &fakeRentals{
		snapshot: rental.FulfillmentSnapshot{
			OrderID: "order-1", ProviderNumberID: "number-1", PhoneNumber: "+14155550001",
			NumberType: "Local", SMSEnabled: true, DurationSeconds: 3600,
		},
		snapshotAfter: rental.FulfillmentSnapshot{
			OrderID: "order-1", ProviderNumberID: "number-2", PhoneNumber: "+14155550002",
			NumberType: "Local", SMSEnabled: true, DurationSeconds: 3600,
		},
		swapNewID: "number-2", swapFound: true,
	}
	failFirst := true
	provider := &failOnceProvider{failOnce: &failFirst}
	svc, err := NewService(rentals, fakeResolver{provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Fulfill(context.Background(), "order-1"); err != nil {
		t.Fatal(err)
	}
	if rentals.activatedWith[1] != "number-2" {
		t.Fatalf("expected activation against the replacement number, got %+v", rentals.activatedWith)
	}
}

// failOnceProvider fails the first ProvisionNumber call, then succeeds.
type failOnceProvider struct {
	fakeProvider
	failOnce *bool
}

func (f *failOnceProvider) ProvisionNumber(ctx context.Context, req telephony.ProvisionNumberRequest) (telephony.Number, error) {
	if *f.failOnce {
		*f.failOnce = false
		return telephony.Number{}, errors.New("provider rejected the number")
	}
	return f.fakeProvider.ProvisionNumber(ctx, req)
}

func TestFulfillReturnsPendingFulfillmentWhenNoReplacementAvailable(t *testing.T) {
	rentals := &fakeRentals{
		snapshot: rental.FulfillmentSnapshot{
			OrderID: "order-1", ProviderNumberID: "number-1", PhoneNumber: "+14155550001",
			NumberType: "Local", SMSEnabled: true, DurationSeconds: 3600,
		},
		swapFound: false,
	}
	provider := &fakeProvider{provisionErr: errors.New("provider rejected the number")}
	svc, err := NewService(rentals, fakeResolver{provider: provider})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Fulfill(context.Background(), "order-1"); !errors.Is(err, ErrPendingFulfillment) {
		t.Fatalf("error = %v", err)
	}
}

func TestNewServiceRequiresDependencies(t *testing.T) {
	if _, err := NewService(nil, fakeResolver{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil rentals error = %v", err)
	}
	if _, err := NewService(&fakeRentals{}, nil); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil resolver error = %v", err)
	}
}
