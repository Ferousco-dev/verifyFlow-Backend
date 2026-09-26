package telephony

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func (t *Twilio) ParseInboundMessage(ctx context.Context, webhook InboundWebhook) (InboundMessage, error) {
	if err := ctx.Err(); err != nil {
		return InboundMessage{}, err
	}
	if len(webhook.Body) > MaxInboundWebhookBytes {
		return InboundMessage{}, fmt.Errorf("%w: inbound webhook body is too large", ErrInvalidRequest)
	}
	parsedURL, err := url.Parse(webhook.PublicURL)
	if err != nil || (parsedURL.Scheme != "http" && parsedURL.Scheme != "https") || parsedURL.Host == "" ||
		parsedURL.User != nil || parsedURL.Fragment != "" {
		return InboundMessage{}, fmt.Errorf("%w: invalid public webhook URL", ErrInvalidRequest)
	}
	if webhook.Signature == "" || !t.validator.ValidateBody(webhook.PublicURL, webhook.Body, webhook.Signature) {
		return InboundMessage{}, ErrInvalidWebhookSignature
	}
	fields, err := url.ParseQuery(string(webhook.Body))
	if err != nil {
		return InboundMessage{}, fmt.Errorf("%w: malformed inbound form", ErrInvalidRequest)
	}
	smsStatus := strings.ToLower(fields.Get("SmsStatus"))
	if fields.Get("MessageStatus") != "" || smsStatus != "" && smsStatus != "received" && smsStatus != "receiving" {
		return InboundMessage{}, fmt.Errorf("%w: delivery status callback is not an inbound message", ErrInvalidRequest)
	}
	if rawMediaCount := fields.Get("NumMedia"); rawMediaCount != "" {
		mediaCount, err := strconv.Atoi(rawMediaCount)
		if err != nil || mediaCount < 0 {
			return InboundMessage{}, fmt.Errorf("%w: invalid media count", ErrInvalidRequest)
		}
		if mediaCount > 0 {
			return InboundMessage{}, fmt.Errorf("%w: inbound media messages are not supported", ErrInvalidRequest)
		}
	}
	providerReference := fields.Get("MessageSid")
	if providerReference == "" {
		providerReference = fields.Get("SmsSid")
	}
	from := strings.TrimSpace(fields.Get("From"))
	to := strings.TrimSpace(fields.Get("To"))
	if providerReference == "" || from == "" || to == "" {
		return InboundMessage{}, fmt.Errorf("%w: inbound message is missing required fields", ErrInvalidRequest)
	}
	return InboundMessage{
		ProviderReference: providerReference,
		From:              from,
		To:                to,
		Body:              fields.Get("Body"),
	}, nil
}
