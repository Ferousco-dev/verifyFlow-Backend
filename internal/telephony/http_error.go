package telephony

import (
	"fmt"
	"net/http"
)

func providerHTTPError(status int) error {
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: HTTP %d", ErrProviderNotFound, status)
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("%w: HTTP %d", ErrProviderRateLimited, status)
	case status >= http.StatusInternalServerError:
		return fmt.Errorf("%w: HTTP %d", ErrProviderUnavailable, status)
	default:
		return fmt.Errorf("%w: HTTP %d", ErrProviderRejected, status)
	}
}
