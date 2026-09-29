package mailer

import (
	"fmt"
	"html"
	"strings"
	"time"
)

// Brand carries the identity every transactional email renders with. Set
// DefaultBrand once at startup (see cmd/api/main.go); until LogoURL points
// at a real hosted image, emails render a plain circle with the brand's
// first letter instead of a broken image, so nothing looks unfinished
// before a logo exists.
type Brand struct {
	Name    string
	LogoURL string
}

// DefaultBrand is read by every template function in this file. It is set
// once, at process startup, from config — never mutated afterward.
var DefaultBrand = Brand{Name: "Verifyflow"}

// esc escapes user-controlled text (names, phone numbers, free-form
// amounts) before it's interpolated into the HTML shell below. Every
// dynamic value that did not come from this codebase's own formatting must
// pass through it.
func esc(s string) string { return html.EscapeString(s) }

// logoBlock renders the circular brand mark at the top of every email: the
// real logo once Brand.LogoURL is set, otherwise a plain black circle with
// the brand's first letter so the layout looks finished either way.
func logoBlock(b Brand) string {
	initial := "M"
	if name := strings.TrimSpace(b.Name); name != "" {
		initial = strings.ToUpper(string([]rune(name)[0]))
	}
	if b.LogoURL != "" {
		return fmt.Sprintf(`<img src="%s" width="64" height="64" alt="%s" style="display:block;margin:0 auto;border-radius:50%%;background:#000000;">`,
			esc(b.LogoURL), esc(b.Name))
	}
	return fmt.Sprintf(`<table role="presentation" align="center" width="64" height="64" style="border-collapse:collapse;"><tr><td align="center" valign="middle" width="64" height="64" style="width:64px;height:64px;border-radius:50%%;background:#000000;color:#ffffff;font-family:Helvetica,Arial,sans-serif;font-size:26px;font-weight:700;">%s</td></tr></table>`,
		esc(initial))
}

// shell wraps bodyHTML (already-safe, pre-escaped HTML built by this
// file's own template functions) in the shared black-and-white chrome:
// logo circle, a thin decorated header rule, the content, and a footer.
// preheader is the short hidden preview text shown next to the subject
// line in most inbox lists.
func shell(b Brand, preheader, bodyHTML string) string {
	return fmt.Sprintf(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>%s</title>
</head>
<body style="margin:0;padding:0;background-color:#f4f4f4;font-family:Helvetica,Arial,sans-serif;">
<span style="display:none;font-size:1px;color:#f4f4f4;line-height:1px;max-height:0;max-width:0;opacity:0;overflow:hidden;">%s</span>
<table role="presentation" width="100%%" cellpadding="0" cellspacing="0" style="background-color:#f4f4f4;padding:32px 16px;">
<tr><td align="center">
<table role="presentation" width="100%%" style="max-width:480px;background-color:#ffffff;border:1px solid #000000;">
<tr><td style="padding:32px 32px 16px 32px;text-align:center;border-bottom:2px solid #000000;">
%s
<div style="margin-top:16px;font-size:13px;letter-spacing:2px;text-transform:uppercase;color:#000000;font-weight:700;">%s</div>
</td></tr>
<tr><td style="padding:32px;color:#111111;font-size:15px;line-height:1.6;">
%s
</td></tr>
<tr><td style="padding:20px 32px;border-top:1px solid #dddddd;color:#777777;font-size:12px;line-height:1.6;text-align:center;">
%s — sent because of activity on your account. If this wasn't you, contact support immediately.
</td></tr>
</table>
</td></tr>
</table>
</body>
</html>`, esc(b.Name), esc(preheader), logoBlock(b), esc(strings.ToUpper(b.Name)), bodyHTML, esc(b.Name))
}

func button(href, label string) string {
	return fmt.Sprintf(`<table role="presentation" cellpadding="0" cellspacing="0" style="margin:24px 0;"><tr><td style="border-radius:4px;background-color:#000000;"><a href="%s" style="display:inline-block;padding:14px 28px;color:#ffffff;text-decoration:none;font-weight:700;font-size:14px;border-radius:4px;">%s</a></td></tr></table>`,
		esc(href), esc(label))
}

func greeting(fullName string) string {
	name := "there"
	if f := strings.Fields(fullName); len(f) > 0 {
		name = f[0]
	}
	return fmt.Sprintf(`<p style="margin:0 0 16px 0;">Hi %s,</p>`, esc(name))
}

// --- Account emails ---

func ResetPasswordEmail(b Brand, to, fullName, link string, ttl time.Duration) Message {
	body := greeting(fullName) +
		`<p style="margin:0 0 8px 0;">We received a request to reset your password. Click below to choose a new one:</p>` +
		button(link, "Reset password") +
		fmt.Sprintf(`<p style="margin:16px 0 0 0;color:#555555;font-size:13px;">This link works once and expires in %d minutes. If you didn't request this, you can ignore this email — your password won't change.</p>`, int(ttl.Minutes()))
	return Message{
		To: to, Subject: fmt.Sprintf("Reset your %s password", b.Name),
		Body: fmt.Sprintf("Hi %s,\n\nWe received a request to reset your password. Open this link:\n%s\n\nThis link works once and expires in %d minutes.\n\n%s", firstName(fullName), link, int(ttl.Minutes()), b.Name),
		HTML: shell(b, "Reset your password", body),
	}
}

func PasswordChangedEmail(b Brand, to, fullName string) Message {
	body := greeting(fullName) +
		`<p style="margin:0;">Your password was just changed, and you were signed out on all devices.</p>` +
		`<p style="margin:16px 0 0 0;color:#555555;font-size:13px;">If this wasn't you, reset your password again immediately and contact support.</p>`
	return Message{
		To: to, Subject: fmt.Sprintf("Your %s password was changed", b.Name),
		Body: fmt.Sprintf("Hi %s,\n\nYour password was just changed, and you were signed out on all devices.\n\nIf this wasn't you, contact support immediately.\n\n%s", firstName(fullName), b.Name),
		HTML: shell(b, "Your password was changed", body),
	}
}

func VerificationEmail(b Brand, to, fullName, link string, ttl time.Duration) Message {
	body := greeting(fullName) +
		fmt.Sprintf(`<p style="margin:0 0 8px 0;">Please confirm your email address to finish setting up your %s account:</p>`, esc(b.Name)) +
		button(link, "Confirm email") +
		fmt.Sprintf(`<p style="margin:16px 0 0 0;color:#555555;font-size:13px;">This link works once and expires in %d hours. If you didn't create this account, you can ignore this email.</p>`, int(ttl.Hours()))
	return Message{
		To: to, Subject: fmt.Sprintf("Confirm your email for %s", b.Name),
		Body: fmt.Sprintf("Hi %s,\n\nConfirm your email address:\n%s\n\nThis link works once and expires in %d hours.\n\n%s", firstName(fullName), link, int(ttl.Hours()), b.Name),
		HTML: shell(b, "Confirm your email address", body),
	}
}

// WelcomeEmail is sent once, right after a customer's email is verified.
func WelcomeEmail(b Brand, to, fullName, dashboardURL string) Message {
	body := greeting(fullName) +
		fmt.Sprintf(`<p style="margin:0 0 8px 0;">Welcome to %s — we're really glad you're here.</p>`, esc(b.Name)) +
		`<p style="margin:0 0 8px 0;">Your account is verified and ready. Fund your wallet, pick a number, and you're set.</p>` +
		button(dashboardURL, "Go to dashboard") +
		fmt.Sprintf(`<p style="margin:24px 0 0 0;color:#555555;font-size:13px;">Thanks for trusting us with this.<br>— The %s founder</p>`, esc(b.Name))
	return Message{
		To: to, Subject: fmt.Sprintf("Welcome to %s", b.Name),
		Body: fmt.Sprintf("Hi %s,\n\nWelcome to %s — we're really glad you're here. Your account is verified and ready:\n%s\n\n— The %s founder", firstName(fullName), b.Name, dashboardURL, b.Name),
		HTML: shell(b, "Welcome aboard", body),
	}
}

// --- Wallet / renewal emails ---

// UpcomingRenewalEmail is sent a few days before a rental's next billing
// period, while there's still time for the customer to top up.
func UpcomingRenewalEmail(b Brand, to, fullName, phoneNumber string, amountMinorUnits int64, currency string, daysUntil int, topUpURL string) Message {
	amount := formatMoney(amountMinorUnits, currency)
	body := greeting(fullName) +
		fmt.Sprintf(`<p style="margin:0 0 8px 0;">In %d day(s), we'll renew <strong>%s</strong> for another billing period and charge <strong>%s</strong> from your %s wallet.</p>`,
			daysUntil, esc(phoneNumber), esc(amount), esc(b.Name)) +
		`<p style="margin:0 0 8px 0;">Make sure your wallet balance covers it — if it doesn't, this number will be released.</p>` +
		button(topUpURL, "Top up wallet")
	return Message{
		To: to, Subject: fmt.Sprintf("%s renews in %d day(s)", phoneNumber, daysUntil),
		Body: fmt.Sprintf("Hi %s,\n\nIn %d day(s) we'll renew %s and charge %s from your %s wallet. Top up here:\n%s\n\n%s",
			firstName(fullName), daysUntil, phoneNumber, amount, b.Name, topUpURL, b.Name),
		HTML: shell(b, fmt.Sprintf("%s renews soon", phoneNumber), body),
	}
}

// RenewalFailedEmail is sent the moment a rental's renewal debit fails
// (insufficient wallet balance, most commonly) and the number is released.
func RenewalFailedEmail(b Brand, to, fullName, phoneNumber, topUpURL string) Message {
	body := greeting(fullName) +
		fmt.Sprintf(`<p style="margin:0 0 8px 0;">Your %s wallet didn't have enough balance to renew <strong>%s</strong>, so it has been released.</p>`, esc(b.Name), esc(phoneNumber)) +
		`<p style="margin:0 0 8px 0;">Top up your wallet and get a new number any time.</p>` +
		button(topUpURL, "Top up wallet")
	return Message{
		To: to, Subject: fmt.Sprintf("%s could not be renewed", phoneNumber),
		Body: fmt.Sprintf("Hi %s,\n\nYour wallet didn't have enough balance to renew %s, so it has been released. Top up here:\n%s\n\n%s",
			firstName(fullName), phoneNumber, topUpURL, b.Name),
		HTML: shell(b, fmt.Sprintf("%s could not be renewed", phoneNumber), body),
	}
}

// --- Receipts ---

// ReceiptLine is one row of a receipt/invoice email (e.g. "Local number —
// one month" / ₦2,500.00).
type ReceiptLine struct {
	Label            string
	AmountMinorUnits int64
}

// ReceiptEmail renders a simple black-and-white receipt for any completed
// payment (wallet top-up, number purchase, renewal). It's sent immediately
// on success. attachmentPDF is optional: pass nil to send the receipt as
// the email body only.
func ReceiptEmail(b Brand, to, fullName, title, reference string, lines []ReceiptLine, totalMinorUnits int64, currency string, issuedAt time.Time, attachmentPDF []byte) Message {
	var rows strings.Builder
	for _, l := range lines {
		rows.WriteString(fmt.Sprintf(
			`<tr><td style="padding:8px 0;border-bottom:1px solid #eeeeee;">%s</td><td style="padding:8px 0;border-bottom:1px solid #eeeeee;text-align:right;">%s</td></tr>`,
			esc(l.Label), esc(formatMoney(l.AmountMinorUnits, currency))))
	}
	body := greeting(fullName) +
		fmt.Sprintf(`<p style="margin:0 0 16px 0;">%s. Here's your receipt.</p>`, esc(title)) +
		fmt.Sprintf(`<table role="presentation" width="100%%" style="border-collapse:collapse;font-size:14px;"><tr><td style="padding-bottom:8px;color:#777777;font-size:12px;">Reference</td><td style="padding-bottom:8px;text-align:right;color:#777777;font-size:12px;">%s</td></tr>`,
			esc(issuedAt.Format("2 Jan 2006, 15:04"))) +
		rows.String() +
		fmt.Sprintf(`<tr><td style="padding-top:12px;font-weight:700;">Total</td><td style="padding-top:12px;text-align:right;font-weight:700;">%s</td></tr></table>`,
			esc(formatMoney(totalMinorUnits, currency))) +
		fmt.Sprintf(`<p style="margin:24px 0 0 0;color:#777777;font-size:12px;">Ref: %s</p>`, esc(reference))

	msg := Message{
		To: to, Subject: fmt.Sprintf("Your %s receipt — %s", b.Name, formatMoney(totalMinorUnits, currency)),
		Body: fmt.Sprintf("Hi %s,\n\n%s. Total: %s. Reference: %s.\n\n%s",
			firstName(fullName), title, formatMoney(totalMinorUnits, currency), reference, b.Name),
		HTML: shell(b, title, body),
	}
	if attachmentPDF != nil {
		msg.Attachments = []Attachment{{Filename: "receipt.pdf", ContentType: "application/pdf", Content: attachmentPDF}}
	}
	return msg
}

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return "there"
}

// formatMoney renders minor units (kobo/cents) as "₦2,500.00" / "$12.34"
// style text. It's deliberately simple — good enough for email copy, not a
// full i18n money formatter.
func formatMoney(minorUnits int64, currency string) string {
	symbol := currency + " "
	switch strings.ToUpper(currency) {
	case "NGN":
		symbol = "₦"
	case "USD":
		symbol = "$"
	case "GBP":
		symbol = "£"
	case "EUR":
		symbol = "€"
	}
	whole := minorUnits / 100
	frac := minorUnits % 100
	if frac < 0 {
		frac = -frac
	}
	return fmt.Sprintf("%s%s.%02d", symbol, groupThousands(whole), frac)
}

func groupThousands(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}
