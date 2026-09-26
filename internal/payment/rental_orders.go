package payment

import (
	"context"
	"errors"

	"migo/internal/rental"
)

// RentalOrders adapts rental.Service to the Orders interface this package
// depends on, translating rental's sentinel errors to payment's.
type RentalOrders struct {
	svc *rental.Service
}

func NewRentalOrders(svc *rental.Service) *RentalOrders { return &RentalOrders{svc: svc} }

func (a *RentalOrders) GetOrder(ctx context.Context, userID, orderID string) (OrderSnapshot, error) {
	return toSnapshot(a.svc.GetOrder(ctx, userID, orderID))
}

func (a *RentalOrders) GetOrderByID(ctx context.Context, orderID string) (OrderSnapshot, error) {
	return toSnapshot(a.svc.GetOrderByID(ctx, orderID))
}

func toSnapshot(o rental.Order, err error) (OrderSnapshot, error) {
	if errors.Is(err, rental.ErrNotFound) {
		return OrderSnapshot{}, ErrOrderNotFound
	}
	if errors.Is(err, rental.ErrInvalidRequest) {
		return OrderSnapshot{}, ErrInvalidRequest
	}
	if err != nil {
		return OrderSnapshot{}, err
	}
	return OrderSnapshot{
		ID: o.ID, UserID: o.UserID, Status: o.Status,
		PriceMinorUnits: o.PriceMinorUnits, Currency: o.Currency,
	}, nil
}
