package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/google/uuid"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/application"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/domain"
)

type Server struct {
	service *application.Service
	logger  *slog.Logger
}

type requestIDKey struct{}

const maxRequestBodyBytes int64 = 1 << 20

func NewHandler(service *application.Service, logger *slog.Logger) http.Handler {
	server := &Server{service: service, logger: logger}
	handler := contract.HandlerWithOptions(server, contract.StdHTTPServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			server.writeError(w, r, domain.NewError(domain.CodeValidation, "request parameters are invalid", nil))
		},
	})
	spec, err := contract.GetSpec()
	if err != nil {
		panic(err)
	}
	validator := nethttpmiddleware.OapiRequestValidatorWithOptions(spec, &nethttpmiddleware.Options{
		Options: openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		ErrorHandlerWithOpts: func(_ context.Context, _ error, w http.ResponseWriter, r *http.Request, options nethttpmiddleware.ErrorHandlerOpts) {
			server.writeErrorStatus(w, r, domain.NewError(domain.CodeValidation, "request does not match OpenAPI contract", nil), options.StatusCode)
		},
		DoNotValidateServers: true,
	})
	return server.withRequestID(server.recoverPanic(server.limitBody(validator(handler))))
}

func (s *Server) Live(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, contract.HealthResponse{Status: "ok"})
}

func (s *Server) Ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.service.Ready(ctx); err != nil {
		s.writeErrorStatus(w, r, err, http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, contract.HealthResponse{Status: "ok"})
}

func (s *Server) ListEstablishments(w http.ResponseWriter, r *http.Request, params contract.ListEstablishmentsParams) {
	limit, offset, err := pagination(params.Limit, params.Offset)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	items, hasMore, err := s.service.ListEstablishments(r.Context(), limit, offset)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := contract.EstablishmentList{
		Items:  make([]contract.Establishment, 0, len(items)),
		Limit:  limit,
		Offset: offset,
	}
	for _, item := range items {
		response.Items = append(response.Items, toContractEstablishment(item))
	}
	if hasMore {
		next := offset + limit
		response.NextOffset = &next
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) GetMenu(w http.ResponseWriter, r *http.Request, establishmentID contract.EstablishmentId) {
	menu, err := s.service.GetMenu(r.Context(), establishmentID.String())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := contract.Menu{
		Establishment: toContractEstablishment(menu.Establishment),
		Items:         make([]contract.MenuItem, 0, len(menu.Items)),
	}
	for _, item := range menu.Items {
		response.Items = append(response.Items, contract.MenuItem{
			Available:   item.Available,
			Currency:    contract.MenuItemCurrencyRUB,
			Description: item.Description,
			ExternalId:  item.ExternalID,
			Id:          uuid.MustParse(item.ID),
			Name:        item.Name,
			PriceMinor:  item.PriceMinor,
			UpdatedAt:   item.UpdatedAt,
		})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) CreateOrder(w http.ResponseWriter, r *http.Request, params contract.CreateOrderParams) {
	var request contract.CreateOrderRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	if request.EstablishmentId == uuid.Nil {
		s.writeError(w, r, domain.NewError(domain.CodeValidation, "establishment_id is required", nil))
		return
	}
	input := domain.BuildOrderInput{
		EstablishmentID: request.EstablishmentId.String(),
		UserID:          request.UserId,
		DeliveryAddress: request.DeliveryAddress,
		Items:           make([]domain.RequestedItem, 0, len(request.Items)),
	}
	for _, item := range request.Items {
		if item.MenuItemId == uuid.Nil {
			s.writeError(w, r, domain.NewError(domain.CodeValidation, "menu_item_id is required", nil))
			return
		}
		input.Items = append(input.Items, domain.RequestedItem{
			MenuItemID:         item.MenuItemId.String(),
			Quantity:           item.Quantity,
			ExpectedPriceMinor: item.ExpectedPriceMinor,
		})
	}
	order, replayed, err := s.service.CreateOrder(r.Context(), input, params.IdempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
		w.Header().Set("Idempotency-Replayed", "true")
	}
	writeJSON(w, status, toContractOrder(order))
}

func (s *Server) GetOrder(w http.ResponseWriter, r *http.Request, orderID contract.OrderId, params contract.GetOrderParams) {
	order, err := s.service.GetOrder(r.Context(), orderID.String(), params.UserId)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContractOrder(order))
}

func (s *Server) ListOrders(w http.ResponseWriter, r *http.Request, params contract.ListOrdersParams) {
	limit, offset, err := pagination(params.Limit, params.Offset)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	orders, hasMore, err := s.service.ListOrders(r.Context(), params.UserId, limit, offset)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := contract.OrderList{Items: make([]contract.Order, 0, len(orders)), Limit: limit, Offset: offset}
	for _, order := range orders {
		response.Items = append(response.Items, toContractOrder(order))
	}
	if hasMore {
		next := offset + limit
		response.NextOffset = &next
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) SyncMenu(w http.ResponseWriter, r *http.Request) {
	establishmentID, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var request contract.MenuSyncRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	items := make([]domain.MenuSyncItem, 0, len(request.Items))
	for _, item := range request.Items {
		if item.Currency != contract.MenuSyncItemCurrencyRUB {
			s.writeError(w, r, domain.NewError(domain.CodeValidation, "currency must be RUB", nil))
			return
		}
		items = append(items, domain.MenuSyncItem{
			ExternalID:  item.ExternalId,
			Name:        item.Name,
			Description: item.Description,
			PriceMinor:  item.PriceMinor,
			Available:   item.Available,
		})
	}
	result, err := s.service.SyncMenu(r.Context(), establishmentID, items)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, contract.MenuSyncResult{
		ActiveItems:      result.ActiveItems,
		DeactivatedItems: result.DeactivatedItems,
		SyncedAt:         result.SyncedAt,
	})
}

func (s *Server) ListIntegrationOrders(w http.ResponseWriter, r *http.Request, params contract.ListIntegrationOrdersParams) {
	establishmentID, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	limit := 20
	if params.Limit != nil {
		limit = *params.Limit
	}
	if limit < 1 || limit > 100 {
		s.writeError(w, r, domain.NewError(domain.CodeValidation, "limit must be between 1 and 100", nil))
		return
	}
	cursor := ""
	if params.Cursor != nil {
		cursor = *params.Cursor
	}
	orders, next, err := s.service.ListIntegrationOrders(r.Context(), establishmentID, cursor, limit)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := contract.IntegrationOrderList{Items: make([]contract.Order, 0, len(orders))}
	for _, order := range orders {
		response.Items = append(response.Items, toContractOrder(order))
	}
	if next != "" {
		response.NextCursor = &next
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) DecideOrder(w http.ResponseWriter, r *http.Request, orderID contract.OrderId) {
	establishmentID, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var request contract.OrderDecisionRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	order, err := s.service.DecideOrder(
		r.Context(),
		establishmentID,
		orderID.String(),
		string(request.Decision),
		request.ExternalOrderId,
		request.Reason,
	)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContractOrder(order))
}

func (s *Server) UpdateOrderStatus(w http.ResponseWriter, r *http.Request, orderID contract.OrderId) {
	establishmentID, err := s.authenticate(r)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	var request contract.OrderStatusRequest
	if err := decodeJSON(w, r, &request); err != nil {
		s.writeError(w, r, err)
		return
	}
	order, err := s.service.UpdateOrderStatus(r.Context(), establishmentID, orderID.String(), string(request.Status))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toContractOrder(order))
}

func (s *Server) authenticate(r *http.Request) (string, error) {
	value := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(value, prefix) {
		return "", domain.NewError(domain.CodeUnauthorized, "integration key is missing or invalid", nil)
	}
	return s.service.AuthenticateEstablishment(r.Context(), strings.TrimSpace(strings.TrimPrefix(value, prefix)))
}

func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		switch domainErr.Code {
		case domain.CodeValidation:
			status = http.StatusBadRequest
		case domain.CodeNotFound, domain.CodeMenuItemNotFound:
			status = http.StatusNotFound
		case domain.CodeUnauthorized:
			status = http.StatusUnauthorized
		default:
			status = http.StatusConflict
		}
	}
	s.writeErrorStatus(w, r, err, status)
}

func (s *Server) writeErrorStatus(w http.ResponseWriter, r *http.Request, err error, status int) {
	requestID := requestIDFromContext(r.Context())
	responseError := contract.Error{Code: "internal_error", Message: "internal server error", RequestId: requestID}
	var domainErr *domain.Error
	if errors.As(err, &domainErr) {
		responseError.Code = string(domainErr.Code)
		responseError.Message = domainErr.Message
		if domainErr.Details != nil {
			details := make(map[string]interface{}, len(domainErr.Details))
			for key, value := range domainErr.Details {
				details[key] = value
			}
			responseError.Details = &details
		}
	} else {
		s.logger.Error("request failed", "request_id", requestID, "method", r.Method, "path", r.URL.Path, "error", err)
	}
	writeJSON(w, status, contract.ErrorResponse{Error: responseError})
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if requestID == "" || len(requestID) > 128 {
			requestID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", requestID)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID)))
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				s.logger.Error("request panicked", "request_id", requestIDFromContext(r.Context()), "panic", recovered)
				s.writeErrorStatus(w, r, errors.New("request panic"), http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func pagination(limitParam, offsetParam *int) (int, int, error) {
	limit, offset := 20, 0
	if limitParam != nil {
		limit = *limitParam
	}
	if offsetParam != nil {
		offset = *offsetParam
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, domain.NewError(domain.CodeValidation, "invalid pagination", nil)
	}
	return limit, offset, nil
}

func decodeJSON(w http.ResponseWriter, r *http.Request, destination any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return domain.NewError(domain.CodeValidation, "request body is invalid", nil)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return domain.NewError(domain.CodeValidation, "request body must contain one JSON object", nil)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func toContractEstablishment(item domain.Establishment) contract.Establishment {
	return contract.Establishment{
		Description: item.Description,
		Id:          uuid.MustParse(item.ID),
		Kind:        contract.EstablishmentKind(item.Kind),
		Name:        item.Name,
	}
}

func toContractOrder(order domain.Order) contract.Order {
	items := make([]contract.OrderItem, 0, len(order.Items))
	for _, item := range order.Items {
		items = append(items, contract.OrderItem{
			ExternalItemId: item.ExternalItemID,
			LineTotalMinor: item.LineTotalMinor,
			MenuItemId:     uuid.MustParse(item.MenuItemID),
			Name:           item.Name,
			Quantity:       item.Quantity,
			UnitPriceMinor: item.UnitPriceMinor,
		})
	}
	return contract.Order{
		CreatedAt:       order.CreatedAt,
		Currency:        contract.OrderCurrencyRUB,
		DeliveryAddress: order.DeliveryAddress,
		EstablishmentId: uuid.MustParse(order.EstablishmentID),
		ExternalOrderId: order.ExternalOrderID,
		Id:              uuid.MustParse(order.ID),
		Items:           items,
		RejectionReason: order.RejectionReason,
		Status:          contract.OrderStatus(order.Status),
		TotalPriceMinor: order.TotalPriceMinor,
		UpdatedAt:       order.UpdatedAt,
		UserId:          order.UserID,
	}
}
