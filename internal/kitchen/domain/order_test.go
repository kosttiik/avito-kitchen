package domain

import (
	"errors"
	"math"
	"testing"
)

func TestBuildOrder(t *testing.T) {
	items := []MenuItem{
		{ID: "item-1", EstablishmentID: "est-1", ExternalID: "pizza", Name: "Pizza", PriceMinor: 129900, Active: true, Available: true},
		{ID: "item-2", EstablishmentID: "est-1", ExternalID: "tea", Name: "Tea", PriceMinor: 25000, Active: true, Available: true},
	}

	tests := []struct {
		name    string
		input   BuildOrderInput
		items   []MenuItem
		want    int64
		wantErr Code
	}{
		{
			name: "builds immutable snapshot and total",
			input: BuildOrderInput{
				EstablishmentID: "est-1",
				UserID:          "user-1",
				DeliveryAddress: "Moscow, Tverskaya 1",
				Items: []RequestedItem{
					{MenuItemID: "item-1", Quantity: 2, ExpectedPriceMinor: 129900},
					{MenuItemID: "item-2", Quantity: 1, ExpectedPriceMinor: 25000},
				},
			},
			items: items,
			want:  284800,
		},
		{
			name:    "rejects unavailable item",
			input:   BuildOrderInput{EstablishmentID: "est-1", UserID: "user-1", DeliveryAddress: "address", Items: []RequestedItem{{MenuItemID: "item-1", Quantity: 1, ExpectedPriceMinor: 129900}}},
			items:   []MenuItem{{ID: "item-1", EstablishmentID: "est-1", PriceMinor: 129900, Active: true, Available: false}},
			wantErr: CodeMenuItemUnavailable,
		},
		{
			name:    "rejects stale price",
			input:   BuildOrderInput{EstablishmentID: "est-1", UserID: "user-1", DeliveryAddress: "address", Items: []RequestedItem{{MenuItemID: "item-1", Quantity: 1, ExpectedPriceMinor: 100}}},
			items:   items[:1],
			wantErr: CodeMenuItemPriceChanged,
		},
		{
			name:    "rejects item from another establishment",
			input:   BuildOrderInput{EstablishmentID: "est-1", UserID: "user-1", DeliveryAddress: "address", Items: []RequestedItem{{MenuItemID: "item-1", Quantity: 1, ExpectedPriceMinor: 129900}}},
			items:   []MenuItem{{ID: "item-1", EstablishmentID: "est-2", PriceMinor: 129900, Active: true, Available: true}},
			wantErr: CodeMixedEstablishments,
		},
		{
			name: "rejects duplicate item",
			input: BuildOrderInput{EstablishmentID: "est-1", UserID: "user-1", DeliveryAddress: "address", Items: []RequestedItem{
				{MenuItemID: "item-1", Quantity: 1, ExpectedPriceMinor: 129900},
				{MenuItemID: "item-1", Quantity: 1, ExpectedPriceMinor: 129900},
			}},
			items:   items[:1],
			wantErr: CodeValidation,
		},
		{
			name:    "rejects overflow",
			input:   BuildOrderInput{EstablishmentID: "est-1", UserID: "user-1", DeliveryAddress: "address", Items: []RequestedItem{{MenuItemID: "item-1", Quantity: 2, ExpectedPriceMinor: math.MaxInt64}}},
			items:   []MenuItem{{ID: "item-1", EstablishmentID: "est-1", PriceMinor: math.MaxInt64, Active: true, Available: true}},
			wantErr: CodeValidation,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			order, err := BuildOrder(tt.input, tt.items)
			if tt.wantErr != "" {
				var domainErr *Error
				if !errors.As(err, &domainErr) || domainErr.Code != tt.wantErr {
					t.Fatalf("BuildOrder() error = %v, want code %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildOrder() error = %v", err)
			}
			if order.TotalPriceMinor != tt.want {
				t.Fatalf("total = %d, want %d", order.TotalPriceMinor, tt.want)
			}
			if order.Items[0].Name != items[0].Name || order.Items[0].UnitPriceMinor != items[0].PriceMinor {
				t.Fatalf("order item is not a menu snapshot: %#v", order.Items[0])
			}
		})
	}
}

func TestNextStatus(t *testing.T) {
	tests := []struct {
		from    OrderStatus
		to      OrderStatus
		wantErr bool
	}{
		{StatusPending, StatusAccepted, false},
		{StatusPending, StatusRejected, false},
		{StatusAccepted, StatusPreparing, false},
		{StatusPreparing, StatusReady, false},
		{StatusAccepted, StatusAccepted, false},
		{StatusReady, StatusAccepted, true},
		{StatusPending, StatusPreparing, true},
		{StatusRejected, StatusAccepted, true},
	}

	for _, tt := range tests {
		err := ValidateTransition(tt.from, tt.to)
		if (err != nil) != tt.wantErr {
			t.Errorf("ValidateTransition(%s, %s) error = %v, wantErr %v", tt.from, tt.to, err, tt.wantErr)
		}
	}
}
