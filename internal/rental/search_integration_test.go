package rental

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func (f rentalFixture) addNumberWithCapabilities(t *testing.T, suffix, numberType string, sms, mms, voice bool) string {
	t.Helper()
	var id string
	phoneNumber := "+1415555" + fmt.Sprintf("%04d", fixturePhoneSequence.Add(1)%10000)
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO provider_numbers (provider_config_id, provider_reference, phone_number, number_type, sms_enabled, mms_enabled, voice_enabled)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7) RETURNING id::text`,
		f.configID, "provider-ref-"+suffix, phoneNumber, numberType, sms, mms, voice,
	).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRepositorySearchNumbersFiltersByStatusTypeAndCapabilities(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	available := f.addNumberWithCapabilities(t, "available", "Local", true, false, false)
	f.addNumberWithCapabilities(t, "wrong-type", "TollFree", true, false, false)
	f.addNumberWithCapabilities(t, "no-sms", "Local", false, false, false)
	reserved := f.addNumberWithCapabilities(t, "reserved", "Local", true, false, false)
	if _, err := f.service.Reserve(context.Background(), f.userID, ReserveRequest{
		ProviderNumberID: reserved, RentalPlanID: f.planID, IdempotencyKey: "reserve-for-search-filter",
	}); err != nil {
		t.Fatal(err)
	}

	page, err := f.service.SearchNumbers(context.Background(), NumberFilter{NumberType: "Local", RequireSMS: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Numbers) != 1 || page.Numbers[0].ID != available {
		t.Fatalf("SearchNumbers = %+v", page)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected next cursor: %s", page.NextCursor)
	}
}

func TestRepositorySearchNumbersPaginates(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, f.addNumberWithCapabilities(t, fmt.Sprintf("page-%d", i), "Local", true, false, false))
	}

	page, err := f.service.SearchNumbers(context.Background(), NumberFilter{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Numbers) != 2 || page.NextCursor == "" {
		t.Fatalf("first page = %+v", page)
	}

	next, err := f.service.SearchNumbers(context.Background(), NumberFilter{Limit: 2, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Numbers) != 1 || next.NextCursor != "" {
		t.Fatalf("second page = %+v", next)
	}
	_ = ids
}

func TestRepositoryListPlansReturnsOnlyActivePlans(t *testing.T) {
	f := newRentalFixture(t, 20*time.Minute)
	var inactiveID string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO rental_plans (plan_code, name, duration_seconds, price_minor_units, currency, is_active)
		 VALUES ('retired', 'Retired plan', 3600, 100, 'USD', false) RETURNING id::text`,
	).Scan(&inactiveID); err != nil {
		t.Fatal(err)
	}

	plans, err := f.service.ListPlans(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range plans {
		if p.ID == inactiveID {
			t.Fatalf("inactive plan leaked into ListPlans: %+v", plans)
		}
	}
	found := false
	for _, p := range plans {
		if p.ID == f.planID {
			found = true
			if p.Code != "one-hour" || p.Currency != "USD" {
				t.Fatalf("plan fields = %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("expected seeded plan in ListPlans: %+v", plans)
	}
}
