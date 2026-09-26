package config

import "testing"

func TestPaystackCallbackURLIsOptional(t *testing.T) {
	cfg, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PaystackCallbackURL != "" {
		t.Fatalf("expected empty PaystackCallbackURL by default, got %q", cfg.PaystackCallbackURL)
	}
}

func TestPaystackCallbackURLValidatedWhenSet(t *testing.T) {
	m := prod(base())
	m["PAYSTACK_CALLBACK_URL"] = "https://app.example.com/paid"
	cfg, err := Load(env(m))
	if err != nil || cfg.PaystackCallbackURL != "https://app.example.com/paid" {
		t.Fatalf("got %q, %v", cfg.PaystackCallbackURL, err)
	}

	bad := map[string]string{
		"fragment":           "https://a.example/paid#x",
		"http in production": "http://a.example/paid",
		"relative":           "/paid",
	}
	for name, v := range bad {
		m := prod(base())
		m["PAYSTACK_CALLBACK_URL"] = v
		if _, err := Load(env(m)); err == nil {
			t.Errorf("%s: expected error for %q", name, v)
		}
	}
}
