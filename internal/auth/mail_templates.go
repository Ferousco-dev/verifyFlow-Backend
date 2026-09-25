package auth

import (
	"fmt"
	"strings"
	"time"

	"migo/internal/mailer"
)

func firstName(full string) string {
	if f := strings.Fields(full); len(f) > 0 {
		return f[0]
	}
	return "there"
}

func resetEmail(to, fullName, link string, ttl time.Duration) mailer.Message {
	return mailer.Message{
		To:      to,
		Subject: "Reset your Migo password",
		Body: fmt.Sprintf(`Hi %s,

We received a request to reset your Migo password. Open the link below to choose a new one:

%s

This link works once and expires in %d minutes. Requesting another link cancels this one.

If you didn't ask for this, you can safely ignore this email. Your password won't change.

Migo
`, firstName(fullName), link, int(ttl.Minutes())),
	}
}

func passwordChangedEmail(to, fullName string) mailer.Message {
	return mailer.Message{
		To:      to,
		Subject: "Your Migo password was changed",
		Body: fmt.Sprintf(`Hi %s,

The password for your Migo account was just changed, and you were signed out on all devices.

If this was you, no action is needed. If it wasn't, reset your password again immediately and contact support.

Migo
`, firstName(fullName)),
	}
}

func verificationEmail(to, fullName, link string, ttl time.Duration) mailer.Message {
	return mailer.Message{
		To:      to,
		Subject: "Confirm your email for Migo",
		Body: fmt.Sprintf(`Hi %s,

Welcome to Migo! Please confirm your email address by opening the link below:

%s

This link works once and expires in %d hours.

If you didn't create a Migo account, you can ignore this email.

Migo
`, firstName(fullName), link, int(ttl.Hours())),
	}
}
