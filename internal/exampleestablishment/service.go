package exampleestablishment

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/contract"
)

type LocalMenuItem struct {
	ExternalID  string `json:"external_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	PriceMinor  int64  `json:"price_minor"`
	Available   bool   `json:"available"`
}

type kitchenClient interface {
	SyncMenuWithResponse(context.Context, contract.SyncMenuJSONRequestBody, ...contract.RequestEditorFn) (*contract.SyncMenuResponse, error)
	ListIntegrationOrdersWithResponse(context.Context, *contract.ListIntegrationOrdersParams, ...contract.RequestEditorFn) (*contract.ListIntegrationOrdersResponse, error)
	DecideOrderWithResponse(context.Context, contract.OrderId, contract.DecideOrderJSONRequestBody, ...contract.RequestEditorFn) (*contract.DecideOrderResponse, error)
	UpdateOrderStatusWithResponse(context.Context, contract.OrderId, contract.UpdateOrderStatusJSONRequestBody, ...contract.RequestEditorFn) (*contract.UpdateOrderStatusResponse, error)
}

type Service struct {
	client       kitchenClient
	logger       *slog.Logger
	pollInterval time.Duration
	mu           sync.RWMutex
	menu         map[string]LocalMenuItem
	orders       map[string]contract.Order
	cursor       string
	ready        atomic.Bool
}

func New(client kitchenClient, logger *slog.Logger, pollInterval time.Duration) *Service {
	return &Service{
		client:       client,
		logger:       logger,
		pollInterval: pollInterval,
		menu: map[string]LocalMenuItem{
			"margherita": {
				ExternalID:  "margherita",
				Name:        "Пицца Маргарита",
				Description: "Томаты, моцарелла и базилик",
				PriceMinor:  69000,
				Available:   true,
			},
			"carbonara": {
				ExternalID:  "carbonara",
				Name:        "Паста Карбонара",
				Description: "Паста, бекон, яйцо и пармезан",
				PriceMinor:  59000,
				Available:   true,
			},
			"lemonade": {
				ExternalID:  "lemonade",
				Name:        "Домашний лимонад",
				Description: "Лимон, мята и газированная вода",
				PriceMinor:  25000,
				Available:   true,
			},
		},
		orders: make(map[string]contract.Order),
	}
}

func (s *Service) Run(ctx context.Context) {
	delay := time.Second
	for {
		if err := s.syncMenu(ctx); err != nil {
			s.logger.Warn("menu sync failed", "error", err, "retry_in", delay)
			if !wait(ctx, delay) {
				return
			}
			delay = minDuration(delay*2, 30*time.Second)
			continue
		}
		s.ready.Store(true)
		break
	}

	delay = s.pollInterval
	for {
		if !wait(ctx, delay) {
			return
		}
		if err := s.pollOrders(ctx); err != nil {
			s.logger.Warn("order polling failed", "error", err, "retry_in", delay)
			delay = minDuration(delay*2, 30*time.Second)
			continue
		}
		delay = s.pollInterval
	}
}

func (s *Service) Ready() bool {
	return s.ready.Load()
}

func (s *Service) Menu() []LocalMenuItem {
	s.mu.RLock()
	defer s.mu.RUnlock()
	items := make([]LocalMenuItem, 0, len(s.menu))
	for _, item := range s.menu {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ExternalID < items[j].ExternalID })
	return items
}

func (s *Service) Orders() []contract.Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	orders := make([]contract.Order, 0, len(s.orders))
	for _, order := range s.orders {
		orders = append(orders, order)
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].CreatedAt.Before(orders[j].CreatedAt) })
	return orders
}

func (s *Service) UpdateMenuItem(ctx context.Context, externalID string, update MenuItemUpdate) (LocalMenuItem, error) {
	s.mu.Lock()
	item, exists := s.menu[externalID]
	if !exists {
		s.mu.Unlock()
		return LocalMenuItem{}, errors.New("menu item not found")
	}
	previous := item
	if update.Name != nil {
		item.Name = *update.Name
	}
	if update.Description != nil {
		item.Description = *update.Description
	}
	if update.PriceMinor != nil {
		item.PriceMinor = *update.PriceMinor
	}
	if update.Available != nil {
		item.Available = *update.Available
	}
	if item.Name == "" || item.PriceMinor < 1 {
		s.mu.Unlock()
		return LocalMenuItem{}, errors.New("invalid menu item update")
	}
	s.menu[externalID] = item
	s.mu.Unlock()

	if err := s.syncMenu(ctx); err != nil {
		s.mu.Lock()
		s.menu[externalID] = previous
		s.mu.Unlock()
		return LocalMenuItem{}, err
	}
	return item, nil
}

func (s *Service) Decide(ctx context.Context, orderID, decision string, reason *string) (contract.Order, error) {
	id, err := uuid.Parse(orderID)
	if err != nil {
		return contract.Order{}, errors.New("invalid order id")
	}
	request := contract.OrderDecisionRequest{
		Decision:        contract.OrderDecisionRequestDecision(decision),
		ExternalOrderId: "example-" + orderID,
		Reason:          reason,
	}
	response, err := s.client.DecideOrderWithResponse(ctx, id, request)
	if err != nil {
		return contract.Order{}, fmt.Errorf("call order decision: %w", err)
	}
	if response.JSON200 == nil {
		return contract.Order{}, responseError("order decision", response.StatusCode(), response.Body)
	}
	s.cacheOrder(*response.JSON200)
	return *response.JSON200, nil
}

func (s *Service) UpdateStatus(ctx context.Context, orderID, status string) (contract.Order, error) {
	id, err := uuid.Parse(orderID)
	if err != nil {
		return contract.Order{}, errors.New("invalid order id")
	}
	request := contract.OrderStatusRequest{Status: contract.OrderStatusRequestStatus(status)}
	response, err := s.client.UpdateOrderStatusWithResponse(ctx, id, request)
	if err != nil {
		return contract.Order{}, fmt.Errorf("call order status: %w", err)
	}
	if response.JSON200 == nil {
		return contract.Order{}, responseError("order status", response.StatusCode(), response.Body)
	}
	s.cacheOrder(*response.JSON200)
	return *response.JSON200, nil
}

func (s *Service) syncMenu(ctx context.Context) error {
	localItems := s.Menu()
	request := contract.MenuSyncRequest{Items: make([]contract.MenuSyncItem, 0, len(localItems))}
	for _, item := range localItems {
		request.Items = append(request.Items, contract.MenuSyncItem{
			Available:   item.Available,
			Currency:    contract.MenuSyncItemCurrencyRUB,
			Description: item.Description,
			ExternalId:  item.ExternalID,
			Name:        item.Name,
			PriceMinor:  item.PriceMinor,
		})
	}
	response, err := s.client.SyncMenuWithResponse(ctx, request)
	if err != nil {
		return fmt.Errorf("call menu sync: %w", err)
	}
	if response.JSON200 == nil {
		return responseError("menu sync", response.StatusCode(), response.Body)
	}
	s.logger.Info("menu synchronized", "active_items", response.JSON200.ActiveItems, "deactivated_items", response.JSON200.DeactivatedItems)
	return nil
}

func (s *Service) pollOrders(ctx context.Context) error {
	s.mu.RLock()
	cursor := s.cursor
	s.mu.RUnlock()
	params := &contract.ListIntegrationOrdersParams{Limit: intPointer(100)}
	if cursor != "" {
		params.Cursor = &cursor
	}
	response, err := s.client.ListIntegrationOrdersWithResponse(ctx, params)
	if err != nil {
		return fmt.Errorf("call order polling: %w", err)
	}
	if response.JSON200 == nil {
		return responseError("order polling", response.StatusCode(), response.Body)
	}
	s.mu.Lock()
	for _, order := range response.JSON200.Items {
		s.orders[order.Id.String()] = order
	}
	if response.JSON200.NextCursor != nil {
		s.cursor = *response.JSON200.NextCursor
	}
	s.mu.Unlock()
	return nil
}

func (s *Service) cacheOrder(order contract.Order) {
	s.mu.Lock()
	s.orders[order.Id.String()] = order
	s.mu.Unlock()
}

func responseError(operation string, status int, body []byte) error {
	return fmt.Errorf("%s returned status %d: %s", operation, status, string(body))
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func minDuration(left, right time.Duration) time.Duration {
	if left < right {
		return left
	}
	return right
}

func intPointer(value int) *int {
	return &value
}
