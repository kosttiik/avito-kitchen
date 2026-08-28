package domain

import (
	"math"
	"strings"
	"time"
)

type OrderStatus string

const (
	StatusPending   OrderStatus = "pending"
	StatusAccepted  OrderStatus = "accepted"
	StatusPreparing OrderStatus = "preparing"
	StatusReady     OrderStatus = "ready"
	StatusRejected  OrderStatus = "rejected"
)

type MenuItem struct {
	ID              string
	EstablishmentID string
	ExternalID      string
	Name            string
	Description     string
	PriceMinor      int64
	Active          bool
	Available       bool
	UpdatedAt       time.Time
}

type RequestedItem struct {
	MenuItemID         string
	Quantity           int
	ExpectedPriceMinor int64
}

type BuildOrderInput struct {
	EstablishmentID string
	UserID          string
	DeliveryAddress string
	Items           []RequestedItem
}

type OrderItem struct {
	MenuItemID     string
	ExternalItemID string
	Name           string
	UnitPriceMinor int64
	Quantity       int
	LineTotalMinor int64
}

type OrderDraft struct {
	EstablishmentID string
	UserID          string
	DeliveryAddress string
	Status          OrderStatus
	TotalPriceMinor int64
	Items           []OrderItem
}

type Order struct {
	ID              string
	EstablishmentID string
	UserID          string
	DeliveryAddress string
	ExternalOrderID *string
	Status          OrderStatus
	RejectionReason *string
	TotalPriceMinor int64
	Items           []OrderItem
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

func BuildOrder(input BuildOrderInput, menuItems []MenuItem) (OrderDraft, error) {
	input.UserID = strings.TrimSpace(input.UserID)
	input.DeliveryAddress = strings.TrimSpace(input.DeliveryAddress)
	if input.EstablishmentID == "" || input.UserID == "" || len(input.UserID) > 128 {
		return OrderDraft{}, NewError(CodeValidation, "invalid order identity", nil)
	}
	if input.DeliveryAddress == "" || len(input.DeliveryAddress) > 500 {
		return OrderDraft{}, NewError(CodeValidation, "invalid delivery address", nil)
	}
	if len(input.Items) == 0 || len(input.Items) > 50 {
		return OrderDraft{}, NewError(CodeValidation, "order must contain between 1 and 50 items", nil)
	}

	menuByID := make(map[string]MenuItem, len(menuItems))
	for _, item := range menuItems {
		menuByID[item.ID] = item
	}

	seen := make(map[string]struct{}, len(input.Items))
	draft := OrderDraft{
		EstablishmentID: input.EstablishmentID,
		UserID:          input.UserID,
		DeliveryAddress: input.DeliveryAddress,
		Status:          StatusPending,
		Items:           make([]OrderItem, 0, len(input.Items)),
	}

	for _, requested := range input.Items {
		if requested.MenuItemID == "" || requested.Quantity < 1 || requested.Quantity > 100 || requested.ExpectedPriceMinor < 1 {
			return OrderDraft{}, NewError(CodeValidation, "invalid order item", map[string]any{"menu_item_id": requested.MenuItemID})
		}
		if _, exists := seen[requested.MenuItemID]; exists {
			return OrderDraft{}, NewError(CodeValidation, "duplicate menu item", map[string]any{"menu_item_id": requested.MenuItemID})
		}
		seen[requested.MenuItemID] = struct{}{}

		item, exists := menuByID[requested.MenuItemID]
		if !exists {
			return OrderDraft{}, NewError(CodeMenuItemNotFound, "menu item not found", map[string]any{"menu_item_id": requested.MenuItemID})
		}
		if item.EstablishmentID != input.EstablishmentID {
			return OrderDraft{}, NewError(CodeMixedEstablishments, "all items must belong to the selected establishment", map[string]any{"menu_item_id": item.ID})
		}
		if !item.Active || !item.Available {
			return OrderDraft{}, NewError(CodeMenuItemUnavailable, "menu item is unavailable", map[string]any{"menu_item_id": item.ID})
		}
		if item.PriceMinor != requested.ExpectedPriceMinor {
			return OrderDraft{}, NewError(CodeMenuItemPriceChanged, "menu item price changed", map[string]any{"menu_item_id": item.ID, "current_price_minor": item.PriceMinor})
		}
		if item.PriceMinor > math.MaxInt64/int64(requested.Quantity) {
			return OrderDraft{}, NewError(CodeValidation, "order total exceeds supported range", nil)
		}
		lineTotal := item.PriceMinor * int64(requested.Quantity)
		if draft.TotalPriceMinor > math.MaxInt64-lineTotal {
			return OrderDraft{}, NewError(CodeValidation, "order total exceeds supported range", nil)
		}
		draft.TotalPriceMinor += lineTotal
		draft.Items = append(draft.Items, OrderItem{
			MenuItemID:     item.ID,
			ExternalItemID: item.ExternalID,
			Name:           item.Name,
			UnitPriceMinor: item.PriceMinor,
			Quantity:       requested.Quantity,
			LineTotalMinor: lineTotal,
		})
	}

	return draft, nil
}

func ValidateTransition(from, to OrderStatus) error {
	if from == to {
		return nil
	}
	allowed := map[OrderStatus]map[OrderStatus]struct{}{
		StatusPending: {
			StatusAccepted: {},
			StatusRejected: {},
		},
		StatusAccepted: {
			StatusPreparing: {},
		},
		StatusPreparing: {
			StatusReady: {},
		},
	}
	if _, ok := allowed[from][to]; !ok {
		return NewError(CodeInvalidOrderTransition, "order status transition is not allowed", map[string]any{"from": from, "to": to})
	}
	return nil
}
