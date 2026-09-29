package messaging

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"migo/internal/telephony"
)

type Repository struct{ pool *pgxpool.Pool }

func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const messageColumns = `id::text, rental_id::text, provider_number_id::text, direction, sender, recipient, body, provider_message_id, status, received_at, sent_at, created_at`

func scanMessage(row pgx.Row) (Message, error) {
	var m Message
	err := row.Scan(&m.ID, &m.RentalID, &m.ProviderNumberID, &m.Direction, &m.Sender, &m.Recipient, &m.Body, &m.ProviderMessageID, &m.Status, &m.ReceivedAt, &m.SentAt, &m.CreatedAt)
	return m, err
}

func (r *Repository) RecordInbound(ctx context.Context, in InboundInput) (Message, bool, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Message{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	payload := map[string]any{"payload_hash_only": true}
	payloadJSON, _ := json.Marshal(payload)
	tag, err := tx.Exec(ctx, `INSERT INTO provider_webhook_events (provider_config_id,provider_kind,idempotency_key,provider_reference,event_type,payload) VALUES ($1::uuid,'telephony',$2,$3,'sms.inbound',$4::jsonb) ON CONFLICT (provider_config_id,idempotency_key) DO NOTHING`, in.ProviderConfigID, "message:"+in.Message.ProviderReference, in.Message.ProviderReference, payloadJSON)
	if err != nil {
		return Message{}, false, err
	}
	if tag.RowsAffected() == 0 {
		return Message{}, false, tx.Commit(ctx)
	}
	row := tx.QueryRow(ctx, `INSERT INTO messages (user_id,rental_id,provider_number_id,provider_config_id,direction,sender,recipient,body,provider_message_id,status,received_at)
		SELECT r.user_id,r.id,pn.id,pn.provider_config_id,'inbound',$3,$4,$5,$6,'received',$7
		FROM provider_numbers pn JOIN rentals r ON r.provider_number_id=pn.id
		WHERE pn.provider_config_id=$1::uuid AND pn.phone_number=$2 AND pn.status='ACTIVE' AND r.ended_at IS NULL AND r.activated_at IS NOT NULL
		RETURNING `+messageColumns, in.ProviderConfigID, in.Message.To, in.Message.From, in.Message.To, in.Message.Body, in.Message.ProviderReference, in.ReceivedAt)
	m, err := scanMessage(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, ErrNotFound
	}
	if err != nil {
		return Message{}, false, err
	}
	if _, err = tx.Exec(ctx, `UPDATE provider_webhook_events SET processed_at=now() WHERE provider_config_id=$1::uuid AND idempotency_key=$2`, in.ProviderConfigID, "message:"+in.Message.ProviderReference); err != nil {
		return Message{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Message{}, false, err
	}
	return m, true, nil
}

func (r *Repository) OutboundTarget(ctx context.Context, userID, numberID string) (OutboundTarget, error) {
	var out OutboundTarget
	err := r.pool.QueryRow(ctx, `SELECT r.id::text,pn.id::text,pn.provider_config_id::text,pn.phone_number FROM provider_numbers pn JOIN rentals r ON r.provider_number_id=pn.id WHERE pn.id=$2::uuid AND r.user_id=$1::uuid AND pn.status='ACTIVE' AND pn.sms_enabled AND r.ended_at IS NULL AND r.activated_at IS NOT NULL AND (r.expires_at IS NULL OR r.expires_at>now())`, userID, numberID).Scan(&out.RentalID, &out.ProviderNumberID, &out.ProviderConfigID, &out.PhoneNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutboundTarget{}, ErrNumberInactive
	}
	return out, err
}

func (r *Repository) RecordOutbound(ctx context.Context, userID string, target OutboundTarget, to, body string, receipt telephony.MessageReceipt, at time.Time) (Message, error) {
	return scanMessage(r.pool.QueryRow(ctx, `INSERT INTO messages (user_id,rental_id,provider_number_id,provider_config_id,direction,sender,recipient,body,provider_message_id,status,sent_at) VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,'outbound',$5,$6,$7,$8,$9,$10) RETURNING `+messageColumns, userID, target.RentalID, target.ProviderNumberID, target.ProviderConfigID, target.PhoneNumber, to, body, receipt.ProviderReference, normalizeStatus(receipt.Status), at))
}

func normalizeStatus(status string) string {
	switch status {
	case "sent", "delivered", "failed", "rejected":
		return status
	default:
		return "queued"
	}
}

func (r *Repository) ListMessages(ctx context.Context, userID, numberID, cursor string, limit int) (Page, error) {
	var before time.Time
	var beforeID string
	if cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return Page{}, ErrInvalidRequest
		}
		var c struct {
			At time.Time `json:"at"`
			ID string    `json:"id"`
		}
		if json.Unmarshal(raw, &c) != nil || c.ID == "" {
			return Page{}, ErrInvalidRequest
		}
		before, beforeID = c.At, c.ID
	}
	rows, err := r.pool.Query(ctx, `SELECT `+messageColumns+` FROM messages WHERE user_id=$1::uuid AND ($2='' OR provider_number_id=$2::uuid) AND ($3::timestamptz IS NULL OR (created_at,id)<($3::timestamptz,$4::uuid)) ORDER BY created_at DESC,id DESC LIMIT $5`, userID, numberID, nullableTime(before), beforeID, limit+1)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()
	out := Page{Messages: []Message{}}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return Page{}, err
		}
		out.Messages = append(out.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(out.Messages) > limit {
		last := out.Messages[limit-1]
		out.Messages = out.Messages[:limit]
		b, _ := json.Marshal(struct {
			At time.Time `json:"at"`
			ID string    `json:"id"`
		}{last.CreatedAt, last.ID})
		out.NextCursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

func (r *Repository) ListNumbers(ctx context.Context, userID string) ([]Number, error) {
	rows, err := r.pool.Query(ctx, `SELECT pn.id::text,r.id::text,pn.phone_number,pn.number_type,pn.status,pn.sms_enabled,pn.mms_enabled,pn.voice_enabled,r.activated_at,r.expires_at FROM rentals r JOIN provider_numbers pn ON pn.id=r.provider_number_id WHERE r.user_id=$1::uuid ORDER BY r.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Number{}
	for rows.Next() {
		var n Number
		if err := rows.Scan(&n.ID, &n.RentalID, &n.PhoneNumber, &n.NumberType, &n.Status, &n.SMSEnabled, &n.MMSEnabled, &n.VoiceEnabled, &n.ActivatedAt, &n.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
