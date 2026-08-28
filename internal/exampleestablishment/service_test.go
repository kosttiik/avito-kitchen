package exampleestablishment

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
)

func TestUpdateMenuItemRollsBackWhenSyncFails(t *testing.T) {
	client := &kitchenClientStub{
		syncMenuFunc: func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
			return nil, errors.New("kitchen unavailable")
		},
	}
	service := New(client, testLogger(), time.Second)
	before := menuItemByID(t, service.Menu(), "margherita")
	price := before.PriceMinor + 1000

	if _, err := service.UpdateMenuItem(context.Background(), before.ExternalID, MenuItemUpdate{PriceMinor: &price}); err == nil {
		t.Fatal("update succeeded while Kitchen API was unavailable")
	}
	after := menuItemByID(t, service.Menu(), before.ExternalID)
	if after != before {
		t.Fatalf("menu update was not rolled back: before=%#v after=%#v", before, after)
	}
}

func TestUpdateMenuItemRejectsInvalidLocalChanges(t *testing.T) {
	service := New(&kitchenClientStub{}, testLogger(), time.Second)
	if _, err := service.UpdateMenuItem(context.Background(), "missing", MenuItemUpdate{}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing item error = %v", err)
	}
	empty := ""
	if _, err := service.UpdateMenuItem(context.Background(), "margherita", MenuItemUpdate{Name: &empty}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("empty name error = %v", err)
	}
	zero := int64(0)
	if _, err := service.UpdateMenuItem(context.Background(), "margherita", MenuItemUpdate{PriceMinor: &zero}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("zero price error = %v", err)
	}
}

func TestPollOrdersDoesNotAdvanceCursorOnFailure(t *testing.T) {
	tests := []struct {
		name   string
		client *kitchenClientStub
	}{
		{
			name: "network error",
			client: &kitchenClientStub{listOrdersFunc: func(context.Context, *contract.ListIntegrationOrdersParams, ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error) {
				return nil, errors.New("connection refused")
			}},
		},
		{
			name: "non 200 response",
			client: &kitchenClientStub{listOrdersFunc: func(context.Context, *contract.ListIntegrationOrdersParams, ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error) {
				return &contract.ListIntegrationOrdersResponse{HTTPResponse: &http.Response{StatusCode: http.StatusUnauthorized}, Body: []byte("unauthorized")}, nil
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := New(test.client, testLogger(), time.Second)
			service.cursor = "cursor-before"
			if err := service.pollOrders(context.Background()); err == nil {
				t.Fatal("poll succeeded")
			}
			if service.cursor != "cursor-before" || len(service.orders) != 0 {
				t.Fatalf("state changed after failed poll: cursor=%q orders=%d", service.cursor, len(service.orders))
			}
		})
	}
}

func TestPollOrdersCachesPageAndAdvancesCursorAfterSuccess(t *testing.T) {
	nextCursor := "cursor-after"
	order := contract.Order{
		Id:              uuid.New(),
		EstablishmentId: uuid.New(),
		UserId:          "user-1",
		DeliveryAddress: "Moscow",
		Status:          contract.OrderStatusPending,
		Currency:        contract.OrderCurrencyRUB,
		TotalPriceMinor: 10000,
		Items:           []contract.OrderItem{},
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	client := &kitchenClientStub{listOrdersFunc: func(_ context.Context, params *contract.ListIntegrationOrdersParams, _ ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error) {
		if params.Cursor == nil || *params.Cursor != "cursor-before" || params.Limit == nil || *params.Limit != 100 {
			t.Fatalf("poll params = %#v", params)
		}
		return &contract.ListIntegrationOrdersResponse{
			HTTPResponse: &http.Response{StatusCode: http.StatusOK},
			JSON200:      &contract.IntegrationOrderList{Items: []contract.Order{order}, NextCursor: &nextCursor},
		}, nil
	}}
	service := New(client, testLogger(), time.Second)
	service.cursor = "cursor-before"

	if err := service.pollOrders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if service.cursor != nextCursor {
		t.Fatalf("cursor = %q", service.cursor)
	}
	cached := service.Orders()
	if len(cached) != 1 || cached[0].Id != order.Id {
		t.Fatalf("cached orders = %#v", cached)
	}
}

func TestRunStopsDuringInitialSyncRetry(t *testing.T) {
	called := make(chan struct{})
	client := &kitchenClientStub{syncMenuFunc: func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
		select {
		case <-called:
		default:
			close(called)
		}
		return nil, errors.New("kitchen unavailable")
	}}
	service := New(client, testLogger(), time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("initial sync was not attempted")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service did not stop during retry delay")
	}
	if service.Ready() {
		t.Fatal("service became ready after failed initial sync")
	}
}

func TestRunRetriesPollingAfterTransientFailure(t *testing.T) {
	var pollCalls atomic.Int32
	polled := make(chan struct{})
	client := &kitchenClientStub{
		syncMenuFunc: func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
			return &contract.SyncMenuResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusOK},
				JSON200:      &contract.MenuSyncResult{},
			}, nil
		},
		listOrdersFunc: func(context.Context, *contract.ListIntegrationOrdersParams, ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error) {
			if pollCalls.Add(1) == 1 {
				return nil, errors.New("temporary polling failure")
			}
			select {
			case <-polled:
			default:
				close(polled)
			}
			return &contract.ListIntegrationOrdersResponse{
				HTTPResponse: &http.Response{StatusCode: http.StatusOK},
				JSON200:      &contract.IntegrationOrderList{Items: []contract.Order{}},
			}, nil
		},
	}
	service := New(client, testLogger(), time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		service.Run(ctx)
		close(done)
	}()
	select {
	case <-polled:
	case <-time.After(time.Second):
		t.Fatal("polling did not recover")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("service did not stop")
	}
	if !service.Ready() || pollCalls.Load() < 2 {
		t.Fatalf("ready=%v poll calls=%d", service.Ready(), pollCalls.Load())
	}
}

func TestSyncMenuRejectsNonOKResponse(t *testing.T) {
	client := &kitchenClientStub{syncMenuFunc: func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
		return &contract.SyncMenuResponse{HTTPResponse: &http.Response{StatusCode: http.StatusBadRequest}, Body: []byte("invalid menu")}, nil
	}}
	service := New(client, testLogger(), time.Second)
	if err := service.syncMenu(context.Background()); err == nil || !strings.Contains(err.Error(), "status 400") {
		t.Fatalf("sync error = %v", err)
	}
}

func TestDecisionAndStatusFailuresDoNotEnterLocalCache(t *testing.T) {
	orderID := uuid.NewString()
	client := &kitchenClientStub{
		decideFunc: func(context.Context, contract.OrderId, contract.DecideOrderJSONRequestBody, ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error) {
			return nil, errors.New("connection refused")
		},
		updateStatusFunc: func(context.Context, contract.OrderId, contract.UpdateOrderStatusJSONRequestBody, ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error) {
			return &contract.UpdateOrderStatusResponse{HTTPResponse: &http.Response{StatusCode: http.StatusConflict}, Body: []byte("invalid transition")}, nil
		},
	}
	service := New(client, testLogger(), time.Second)
	if _, err := service.Decide(context.Background(), orderID, "accepted", nil); err == nil || !strings.Contains(err.Error(), "call order decision") {
		t.Fatalf("decision error = %v", err)
	}
	if _, err := service.UpdateStatus(context.Background(), orderID, "preparing"); err == nil || !strings.Contains(err.Error(), "status 409") {
		t.Fatalf("status error = %v", err)
	}
	if len(service.Orders()) != 0 {
		t.Fatalf("failed operations entered cache: %#v", service.Orders())
	}
	if _, err := service.Decide(context.Background(), "not-a-uuid", "accepted", nil); err == nil || !strings.Contains(err.Error(), "invalid order id") {
		t.Fatalf("invalid decision id error = %v", err)
	}
	if _, err := service.UpdateStatus(context.Background(), "not-a-uuid", "preparing"); err == nil || !strings.Contains(err.Error(), "invalid order id") {
		t.Fatalf("invalid status id error = %v", err)
	}
}

func TestDecisionAndStatusSuccessesRefreshLocalCache(t *testing.T) {
	orderID := uuid.New()
	order := contract.Order{
		Id:              orderID,
		EstablishmentId: uuid.New(),
		UserId:          "user-1",
		DeliveryAddress: "Moscow",
		Status:          contract.OrderStatusAccepted,
		Currency:        contract.OrderCurrencyRUB,
		TotalPriceMinor: 10000,
		Items:           []contract.OrderItem{},
		CreatedAt:       time.Now(),
		UpdatedAt:       time.Now(),
	}
	client := &kitchenClientStub{
		decideFunc: func(_ context.Context, id contract.OrderId, request contract.DecideOrderJSONRequestBody, _ ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error) {
			if id != orderID || request.Decision != contract.OrderDecisionRequestDecisionAccepted || request.ExternalOrderId != "example-"+orderID.String() {
				t.Fatalf("decision request: id=%s body=%#v", id, request)
			}
			return &contract.DecideOrderResponse{HTTPResponse: &http.Response{StatusCode: http.StatusOK}, JSON200: &order}, nil
		},
		updateStatusFunc: func(_ context.Context, id contract.OrderId, request contract.UpdateOrderStatusJSONRequestBody, _ ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error) {
			if id != orderID || request.Status != contract.OrderStatusRequestStatusPreparing {
				t.Fatalf("status request: id=%s body=%#v", id, request)
			}
			updated := order
			updated.Status = contract.OrderStatusPreparing
			return &contract.UpdateOrderStatusResponse{HTTPResponse: &http.Response{StatusCode: http.StatusOK}, JSON200: &updated}, nil
		},
	}
	service := New(client, testLogger(), time.Second)
	if _, err := service.Decide(context.Background(), orderID.String(), "accepted", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UpdateStatus(context.Background(), orderID.String(), "preparing"); err != nil {
		t.Fatal(err)
	}
	cached := service.Orders()
	if len(cached) != 1 || cached[0].Id != orderID || cached[0].Status != contract.OrderStatusPreparing {
		t.Fatalf("cached orders = %#v", cached)
	}
}

type kitchenClientStub struct {
	syncMenuFunc     func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error)
	listOrdersFunc   func(context.Context, *contract.ListIntegrationOrdersParams, ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error)
	decideFunc       func(context.Context, contract.OrderId, contract.DecideOrderJSONRequestBody, ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error)
	updateStatusFunc func(context.Context, contract.OrderId, contract.UpdateOrderStatusJSONRequestBody, ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error)
}

func (c *kitchenClientStub) SyncMenuWithResponse(ctx context.Context, body contract.SyncMenuJSONRequestBody, editors ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
	if c.syncMenuFunc == nil {
		return nil, errors.New("unexpected menu sync")
	}
	return c.syncMenuFunc(ctx, body, editors...)
}

func (c *kitchenClientStub) ListIntegrationOrdersWithResponse(ctx context.Context, params *contract.ListIntegrationOrdersParams, editors ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error) {
	if c.listOrdersFunc == nil {
		return nil, errors.New("unexpected order polling")
	}
	return c.listOrdersFunc(ctx, params, editors...)
}

func (c *kitchenClientStub) DecideOrderWithResponse(ctx context.Context, orderID contract.OrderId, body contract.DecideOrderJSONRequestBody, editors ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error) {
	if c.decideFunc == nil {
		return nil, errors.New("unexpected order decision")
	}
	return c.decideFunc(ctx, orderID, body, editors...)
}

func (c *kitchenClientStub) UpdateOrderStatusWithResponse(ctx context.Context, orderID contract.OrderId, body contract.UpdateOrderStatusJSONRequestBody, editors ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error) {
	if c.updateStatusFunc == nil {
		return nil, errors.New("unexpected order status")
	}
	return c.updateStatusFunc(ctx, orderID, body, editors...)
}

func menuItemByID(t *testing.T, items []LocalMenuItem, externalID string) LocalMenuItem {
	t.Helper()
	for _, item := range items {
		if item.ExternalID == externalID {
			return item
		}
	}
	t.Fatalf("menu item %q not found", externalID)
	return LocalMenuItem{}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
