package mailer

import (
	"strings"
	"testing"
	"time"
)

func TestTemplatesEscapeUserControlledText(t *testing.T) {
	b := Brand{Name: "Migo"}
	evil := `<img src=x onerror=alert(1)>`
	msg := WelcomeEmail(b, "user@example.com", evil, "https://app.example.com/dashboard")
	if strings.Contains(msg.HTML, "<img src=x onerror") {
		t.Fatalf("full name was not escaped in HTML body: %s", msg.HTML)
	}
	if !strings.Contains(msg.HTML, "&lt;img") {
		t.Fatalf("expected escaped angle brackets in HTML body: %s", msg.HTML)
	}
}

func TestLogoBlockFallsBackToInitialWithoutLogoURL(t *testing.T) {
	b := Brand{Name: "Migo"}
	got := logoBlock(b)
	if !strings.Contains(got, ">M<") {
		t.Fatalf("expected a circle with the brand initial when LogoURL is empty, got: %s", got)
	}
	if strings.Contains(got, "<img") {
		t.Fatalf("should not render an <img> tag without a LogoURL: %s", got)
	}
}

func TestLogoBlockUsesImageWhenLogoURLSet(t *testing.T) {
	b := Brand{Name: "Migo", LogoURL: "https://cdn.example.com/logo.png"}
	got := logoBlock(b)
	if !strings.Contains(got, `src="https://cdn.example.com/logo.png"`) {
		t.Fatalf("expected an <img> pointing at LogoURL, got: %s", got)
	}
}

func TestFormatMoneyNairaAndDollar(t *testing.T) {
	cases := []struct {
		minor    int64
		currency string
		want     string
	}{
		{250000, "NGN", "₦2,500.00"},
		{99, "USD", "$0.99"},
		{100000000, "NGN", "₦1,000,000.00"},
	}
	for _, c := range cases {
		if got := formatMoney(c.minor, c.currency); got != c.want {
			t.Fatalf("formatMoney(%d, %s) = %q, want %q", c.minor, c.currency, got, c.want)
		}
	}
}

func TestReceiptEmailIncludesAttachmentOnlyWhenGiven(t *testing.T) {
	b := Brand{Name: "Migo"}
	lines := []ReceiptLine{{Label: "Local number — one month", AmountMinorUnits: 250000}}

	withoutPDF := ReceiptEmail(b, "user@example.com", "Ada Lovelace", "Payment received", "ref-1", lines, 250000, "NGN", time.Now(), nil)
	if len(withoutPDF.Attachments) != 0 {
		t.Fatalf("expected no attachments when attachmentPDF is nil")
	}

	withPDF := ReceiptEmail(b, "user@example.com", "Ada Lovelace", "Payment received", "ref-1", lines, 250000, "NGN", time.Now(), []byte("%PDF-1.4"))
	if len(withPDF.Attachments) != 1 || withPDF.Attachments[0].ContentType != "application/pdf" {
		t.Fatalf("expected one PDF attachment, got %+v", withPDF.Attachments)
	}
}
