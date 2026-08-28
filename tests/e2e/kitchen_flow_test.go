//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
)

func TestUserAndEstablishmentJourney(t *testing.T) {
	kitchenURL := envOrDefault("KITCHEN_API_URL", "http://localhost:8080")
	establishmentURL := envOrDefault("ESTABLISHMENT_API_URL", "http://localhost:8081")
	client, err := contract.NewClientWithResponses(kitchenURL, contract.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	waitForStatus(t, ctx, kitchenURL+"/health/ready", http.StatusOK)
	waitForStatus(t, ctx, establishmentURL+"/health/ready", http.StatusOK)

	establishments, err := client.ListEstablishmentsWithResponse(ctx, &contract.ListEstablishmentsParams{})
	if err != nil || establishments.JSON200 == nil || len(establishments.JSON200.Items) == 0 {
		t.Fatalf("list establishments: response=%#v err=%v", establishments, err)
	}
	establishment := establishments.JSON200.Items[0]

	menuResponse, err := client.GetMenuWithResponse(ctx, establishment.Id)
	if err != nil || menuResponse.JSON200 == nil || len(menuResponse.JSON200.Items) == 0 {
		t.Fatalf("get menu: response=%#v err=%v", menuResponse, err)
	}
	var menuItem contract.MenuItem
	for _, item := range menuResponse.JSON200.Items {
		if item.ExternalId == "margherita" {
			menuItem = item
			break
		}
	}
	if menuItem.ExternalId == "" || !menuItem.Available {
		t.Fatalf("demo menu item is not available: %#v", menuItem)
	}

	userID := "e2e-user-" + uuid.NewString()
	idempotencyKey := "e2e-order-" + uuid.NewString()
	createRequest := contract.CreateOrderRequest{
		DeliveryAddress: "Moscow, Tverskaya Street, 1",
		EstablishmentId: establishment.Id,
		UserId:          userID,
		Items: []contract.CreateOrderItem{{
			ExpectedPriceMinor: menuItem.PriceMinor,
			MenuItemId:         menuItem.Id,
			Quantity:           2,
		}},
	}
	created, err := client.CreateOrderWithResponse(ctx, &contract.CreateOrderParams{IdempotencyKey: idempotencyKey}, createRequest)
	if err != nil || created.JSON201 == nil {
		t.Fatalf("create order: status=%v body=%s err=%v", statusCode(created), responseBody(created), err)
	}
	order := *created.JSON201
	if order.Status != contract.OrderStatusPending || order.TotalPriceMinor != menuItem.PriceMinor*2 {
		t.Fatalf("unexpected order: %#v", order)
	}

	replayed, err := client.CreateOrderWithResponse(ctx, &contract.CreateOrderParams{IdempotencyKey: idempotencyKey}, createRequest)
	if err != nil || replayed.JSON200 == nil || replayed.JSON200.Id != order.Id {
		t.Fatalf("idempotent replay: status=%v body=%s err=%v", statusCode(replayed), responseBody(replayed), err)
	}

	waitForOrder(t, ctx, establishmentURL, order.Id.String())
	postJSON(t, ctx, establishmentURL+"/operator/v1/orders/"+order.Id.String()+"/decision", map[string]any{"decision": "accepted"}, http.StatusOK)
	patchJSON(t, ctx, establishmentURL+"/operator/v1/orders/"+order.Id.String()+"/status", map[string]any{"status": "preparing"}, http.StatusOK)
	patchJSON(t, ctx, establishmentURL+"/operator/v1/orders/"+order.Id.String()+"/status", map[string]any{"status": "ready"}, http.StatusOK)

	tracked, err := client.GetOrderWithResponse(ctx, order.Id, &contract.GetOrderParams{UserId: userID})
	if err != nil || tracked.JSON200 == nil || tracked.JSON200.Status != contract.OrderStatusReady {
		t.Fatalf("track ready order: status=%v body=%s err=%v", statusCode(tracked), responseBody(tracked), err)
	}
	if tracked.JSON200.Items[0].Name != menuItem.Name || tracked.JSON200.Items[0].UnitPriceMinor != menuItem.PriceMinor {
		t.Fatalf("order snapshot mismatch: %#v", tracked.JSON200.Items[0])
	}

	rejectedRequest := createRequest
	rejectedRequest.UserId = "e2e-user-" + uuid.NewString()
	rejectedRequest.Items[0].Quantity = 1
	rejected, err := client.CreateOrderWithResponse(
		ctx,
		&contract.CreateOrderParams{IdempotencyKey: "e2e-order-" + uuid.NewString()},
		rejectedRequest,
	)
	if err != nil || rejected.JSON201 == nil {
		t.Fatalf("create rejected-path order: status=%v body=%s err=%v", statusCode(rejected), responseBody(rejected), err)
	}
	waitForOrder(t, ctx, establishmentURL, rejected.JSON201.Id.String())
	postJSON(
		t,
		ctx,
		establishmentURL+"/operator/v1/orders/"+rejected.JSON201.Id.String()+"/decision",
		map[string]any{"decision": "rejected", "reason": "out_of_stock"},
		http.StatusOK,
	)
	rejectedTracked, err := client.GetOrderWithResponse(ctx, rejected.JSON201.Id, &contract.GetOrderParams{UserId: rejectedRequest.UserId})
	if err != nil || rejectedTracked.JSON200 == nil || rejectedTracked.JSON200.Status != contract.OrderStatusRejected || rejectedTracked.JSON200.RejectionReason == nil || *rejectedTracked.JSON200.RejectionReason != "out_of_stock" {
		t.Fatalf("track rejected order: status=%v body=%s err=%v", statusCode(rejectedTracked), responseBody(rejectedTracked), err)
	}

	patchJSON(t, ctx, establishmentURL+"/operator/v1/menu-items/margherita", map[string]any{"available": false}, http.StatusOK)
	waitForUnavailable(t, ctx, client, establishment.Id, menuItem.Id)

	unavailableRequest := createRequest
	unavailableRequest.UserId = "e2e-user-" + uuid.NewString()
	conflict, err := client.CreateOrderWithResponse(
		ctx,
		&contract.CreateOrderParams{IdempotencyKey: "e2e-order-" + uuid.NewString()},
		unavailableRequest,
	)
	if err != nil || conflict.JSON409 == nil || conflict.JSON409.Error.Code != "menu_item_unavailable" {
		t.Fatalf("unavailable item: status=%v body=%s err=%v", statusCode(conflict), responseBody(conflict), err)
	}

	patchJSON(t, ctx, establishmentURL+"/operator/v1/menu-items/margherita", map[string]any{"available": true}, http.StatusOK)
}

func waitForStatus(t *testing.T, ctx context.Context, url string, expected int) {
	t.Helper()
	waitUntil(t, ctx, func() bool {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == expected
	})
}

func waitForOrder(t *testing.T, ctx context.Context, establishmentURL, orderID string) {
	t.Helper()
	waitUntil(t, ctx, func() bool {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, establishmentURL+"/operator/v1/orders", nil)
		if err != nil {
			return false
		}
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		var payload struct {
			Items []contract.Order `json:"items"`
		}
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&payload) != nil {
			return false
		}
		for _, order := range payload.Items {
			if order.Id.String() == orderID {
				return true
			}
		}
		return false
	})
}

func waitForUnavailable(t *testing.T, ctx context.Context, client *contract.ClientWithResponses, establishmentID, menuItemID uuid.UUID) {
	t.Helper()
	waitUntil(t, ctx, func() bool {
		response, err := client.GetMenuWithResponse(ctx, establishmentID)
		if err != nil || response.JSON200 == nil {
			return false
		}
		for _, item := range response.JSON200.Items {
			if item.Id == menuItemID {
				return !item.Available
			}
		}
		return false
	})
}

func waitUntil(t *testing.T, ctx context.Context, predicate func() bool) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		if predicate() {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func postJSON(t *testing.T, ctx context.Context, url string, payload any, status int) {
	t.Helper()
	requestJSON(t, ctx, http.MethodPost, url, payload, status)
}

func patchJSON(t *testing.T, ctx context.Context, url string, payload any, status int) {
	t.Helper()
	requestJSON(t, ctx, http.MethodPatch, url, payload, status)
}

func requestJSON(t *testing.T, ctx context.Context, method, url string, payload any, expectedStatus int) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != expectedStatus {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("%s %s status=%d body=%s", method, url, response.StatusCode, responseBody)
	}
}

func envOrDefault(name, fallback string) string {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	return value
}

type responseWithStatus interface {
	StatusCode() int
	GetBody() []byte
}

func statusCode(response responseWithStatus) int {
	if response == nil {
		return 0
	}
	return response.StatusCode()
}

func responseBody(response responseWithStatus) string {
	if response == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s", response.GetBody())
}
