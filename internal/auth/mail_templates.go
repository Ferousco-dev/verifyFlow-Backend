package auth

import (
	"time"

	"migo/internal/mailer"
)

func resetEmail(to, fullName, link string, ttl time.Duration) mailer.Message {
	return mailer.ResetPasswordEmail(mailer.DefaultBrand, to, fullName, link, ttl)
}

func passwordChangedEmail(to, fullName string) mailer.Message {
	return mailer.PasswordChangedEmail(mailer.DefaultBrand, to, fullName)
}

func verificationEmail(to, fullName, link string, ttl time.Duration) mailer.Message {
	return mailer.VerificationEmail(mailer.DefaultBrand, to, fullName, link, ttl)
}
