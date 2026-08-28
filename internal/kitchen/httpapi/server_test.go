package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/application"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/domain"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/httpapi"
)

func TestCreateOrderRejectsMissingEstablishmentID(t *testing.T) {
	handler := testHandler(&repositoryStub{})
	body := `{"user_id":"user-1","delivery_address":"Moscow","items":[{"menu_item_id":"22222222-2222-4222-8222-222222222222","quantity":1,"expected_price_minor":100}]}`
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "order-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "validation_error")
}

func TestListEstablishmentsUsesContractPagination(t *testing.T) {
	id := uuid.NewString()
	handler := testHandler(&repositoryStub{
		establishments: []domain.Establishment{{ID: id, Name: "Demo", Kind: "restaurant", Description: "Menu"}},
		hasMore:        true,
	})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/establishments?limit=1&offset=2", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload contract.EstablishmentList
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Id.String() != id || payload.NextOffset == nil || *payload.NextOffset != 3 {
		t.Fatalf("response = %#v", payload)
	}
}

func TestListOrdersReturnsPaginatedUserHistory(t *testing.T) {
	firstID := uuid.NewString()
	secondID := uuid.NewString()
	repository := &repositoryStub{
		orders: []domain.Order{
			{ID: firstID, EstablishmentID: uuid.NewString(), UserID: "user-1", DeliveryAddress: "Moscow", Status: domain.StatusReady, TotalPriceMinor: 20000, CreatedAt: time.Now(), UpdatedAt: time.Now()},
			{ID: secondID, EstablishmentID: uuid.NewString(), UserID: "user-1", DeliveryAddress: "Moscow", Status: domain.StatusPending, TotalPriceMinor: 10000, CreatedAt: time.Now(), UpdatedAt: time.Now()},
		},
		ordersHasMore: true,
	}
	handler := testHandler(repository)
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/orders?user_id=user-1&limit=2&offset=3", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var payload contract.OrderList
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Items) != 2 || payload.Items[0].Id.String() != firstID || payload.Items[1].Id.String() != secondID {
		t.Fatalf("items = %#v", payload.Items)
	}
	if payload.Limit != 2 || payload.Offset != 3 || payload.NextOffset == nil || *payload.NextOffset != 5 {
		t.Fatalf("pagination = %#v", payload)
	}
	if repository.listOrdersUserID != "user-1" || repository.listOrdersLimit != 2 || repository.listOrdersOffset != 3 {
		t.Fatalf("repository arguments: user=%q limit=%d offset=%d", repository.listOrdersUserID, repository.listOrdersLimit, repository.listOrdersOffset)
	}
}

func TestCreateOrderRejectsMalformedBody(t *testing.T) {
	handler := testHandler(&repositoryStub{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/orders", bytes.NewBufferString(`{"user_id":`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "order-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "validation_error")
}

func TestOpenAPIValidatorRejectsUnsupportedContentType(t *testing.T) {
	handler := testHandler(&repositoryStub{})
	body := `{"user_id":"user-1","establishment_id":"11111111-1111-4111-8111-111111111111","delivery_address":"Moscow","items":[{"menu_item_id":"22222222-2222-4222-8222-222222222222","quantity":1,"expected_price_minor":100}]}`
	request := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/v1/orders", strings.NewReader(body))
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("Idempotency-Key", "order-1")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "validation_error")
}

func TestIntegrationAPIRequiresBearerKey(t *testing.T) {
	handler := testHandler(&repositoryStub{})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/integration/v1/orders", nil)
	request.Header.Set("X-Request-ID", "request-from-test")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	errorResponse := assertAPIError(t, response, http.StatusUnauthorized, "unauthorized")
	if errorResponse.Error.RequestId != "request-from-test" {
		t.Fatalf("request id = %q", errorResponse.Error.RequestId)
	}
}

func TestIntegrationAPIRejectsForgedCursor(t *testing.T) {
	handler := testHandler(&repositoryStub{authenticatedEstablishmentID: uuid.NewString()})
	cursor := base64.RawURLEncoding.EncodeToString([]byte(`{"updated_at":"2026-08-27T12:00:00Z","order_id":"not-a-uuid"}`))
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/integration/v1/orders?cursor="+cursor, nil)
	request.Header.Set("Authorization", "Bearer test-key")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusBadRequest, "validation_error")
}

func TestInternalErrorDoesNotLeakDetails(t *testing.T) {
	handler := testHandler(&repositoryStub{listErr: errors.New("postgres password=secret")})
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/establishments", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	assertAPIError(t, response, http.StatusInternalServerError, "internal_error")
	if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("internal details leaked: %s", response.Body.String())
	}
}

func testHandler(repository application.Repository) http.Handler {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return httpapi.NewHandler(application.New(repository), logger)
}

func assertAPIError(t *testing.T, response *httptest.ResponseRecorder, expectedStatus int, expectedCode string) contract.ErrorResponse {
	t.Helper()
	if response.Code != expectedStatus {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	if contentType := response.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("content type = %q", contentType)
	}
	var payload contract.ErrorResponse
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != expectedCode {
		t.Fatalf("error code = %q", payload.Error.Code)
	}
	if payload.Error.RequestId == "" {
		t.Fatal("request id is empty")
	}
	return payload
}

type repositoryStub struct {
	establishments               []domain.Establishment
	hasMore                      bool
	listErr                      error
	authenticatedEstablishmentID string
	orders                       []domain.Order
	ordersHasMore                bool
	listOrdersErr                error
	listOrdersUserID             string
	listOrdersLimit              int
	listOrdersOffset             int
}

func (r *repositoryStub) Ping(context.Context) error {
	return nil
}

func (r *repositoryStub) ListEstablishments(context.Context, int, int) ([]domain.Establishment, bool, error) {
	return r.establishments, r.hasMore, r.listErr
}

func (r *repositoryStub) GetMenu(context.Context, string) (domain.Menu, error) {
	return domain.Menu{}, errors.New("unexpected GetMenu call")
}

func (r *repositoryStub) AuthenticateEstablishment(context.Context, string) (string, error) {
	if r.authenticatedEstablishmentID != "" {
		return r.authenticatedEstablishmentID, nil
	}
	return "", errors.New("unexpected AuthenticateEstablishment call")
}

func (r *repositoryStub) SyncMenu(context.Context, string, []domain.MenuSyncItem) (domain.MenuSyncResult, error) {
	return domain.MenuSyncResult{}, errors.New("unexpected SyncMenu call")
}

func (r *repositoryStub) CreateOrder(context.Context, domain.BuildOrderInput, string, [32]byte) (domain.Order, bool, error) {
	return domain.Order{}, false, errors.New("unexpected CreateOrder call")
}

func (r *repositoryStub) GetOrder(context.Context, string, string) (domain.Order, error) {
	return domain.Order{}, errors.New("unexpected GetOrder call")
}

func (r *repositoryStub) ListOrders(_ context.Context, userID string, limit, offset int) ([]domain.Order, bool, error) {
	r.listOrdersUserID = userID
	r.listOrdersLimit = limit
	r.listOrdersOffset = offset
	return r.orders, r.ordersHasMore, r.listOrdersErr
}

func (r *repositoryStub) ListIntegrationOrders(context.Context, string, *application.Cursor, int) ([]domain.Order, *application.Cursor, error) {
	return nil, nil, errors.New("unexpected ListIntegrationOrders call")
}

func (r *repositoryStub) DecideOrder(context.Context, string, string, domain.OrderStatus, string, *string) (domain.Order, error) {
	return domain.Order{}, errors.New("unexpected DecideOrder call")
}

func (r *repositoryStub) UpdateOrderStatus(context.Context, string, string, domain.OrderStatus) (domain.Order, error) {
	return domain.Order{}, errors.New("unexpected UpdateOrderStatus call")
}
