package exampleestablishment

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
)

func TestOperatorHTTPFailurePaths(t *testing.T) {
	client := &kitchenClientStub{
		syncMenuFunc: func(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error) {
			return nil, errors.New("kitchen unavailable")
		},
		decideFunc: func(context.Context, contract.OrderId, contract.DecideOrderJSONRequestBody, ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error) {
			return nil, errors.New("kitchen unavailable")
		},
		updateStatusFunc: func(context.Context, contract.OrderId, contract.UpdateOrderStatusJSONRequestBody, ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error) {
			return &contract.UpdateOrderStatusResponse{HTTPResponse: &http.Response{StatusCode: http.StatusConflict}, Body: []byte("invalid transition")}, nil
		},
	}
	handler := NewHandler(New(client, testLogger(), time.Second))
	orderID := uuid.NewString()
	tests := []struct {
		name   string
		method string
		path   string
		body   string
		status int
	}{
		{name: "not ready", method: http.MethodGet, path: "/health/ready", status: http.StatusServiceUnavailable},
		{name: "malformed menu update", method: http.MethodPatch, path: "/operator/v1/menu-items/margherita", body: `{"available":`, status: http.StatusBadRequest},
		{name: "empty menu update", method: http.MethodPatch, path: "/operator/v1/menu-items/margherita", body: `{}`, status: http.StatusBadRequest},
		{name: "unknown menu item", method: http.MethodPatch, path: "/operator/v1/menu-items/missing", body: `{"available":false}`, status: http.StatusNotFound},
		{name: "invalid menu item", method: http.MethodPatch, path: "/operator/v1/menu-items/margherita", body: `{"price_minor":0}`, status: http.StatusBadRequest},
		{name: "menu sync unavailable", method: http.MethodPatch, path: "/operator/v1/menu-items/margherita", body: `{"available":false}`, status: http.StatusBadGateway},
		{name: "decision unavailable", method: http.MethodPost, path: "/operator/v1/orders/" + orderID + "/decision", body: `{"decision":"accepted"}`, status: http.StatusBadGateway},
		{name: "status rejected upstream", method: http.MethodPatch, path: "/operator/v1/orders/" + orderID + "/status", body: `{"status":"preparing"}`, status: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var body *strings.Reader
			if test.body == "" {
				body = strings.NewReader("")
			} else {
				body = strings.NewReader(test.body)
			}
			request := httptest.NewRequestWithContext(context.Background(), test.method, test.path, body)
			if test.body != "" {
				request.Header.Set("Content-Type", "application/json")
			}
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != test.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if response.Header().Get("Content-Type") != "application/json" {
				t.Fatalf("content type=%q", response.Header().Get("Content-Type"))
			}
		})
	}
}

func TestOperatorHTTPListsLocalState(t *testing.T) {
	service := New(&kitchenClientStub{}, testLogger(), time.Second)
	service.ready.Store(true)
	handler := NewHandler(service)
	for _, path := range []string{"/health/live", "/health/ready", "/operator/v1/menu-items", "/operator/v1/orders"} {
		request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
		response := httptest.NewRecorder()

		handler.ServeHTTP(response, request)

		if response.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
}
