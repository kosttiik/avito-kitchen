package application

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/domain"
)

type Cursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	OrderID   string    `json:"order_id"`
}

type Repository interface {
	Ping(context.Context) error
	ListEstablishments(context.Context, int, int) ([]domain.Establishment, bool, error)
	GetMenu(context.Context, string) (domain.Menu, error)
	AuthenticateEstablishment(context.Context, string) (string, error)
	SyncMenu(context.Context, string, []domain.MenuSyncItem) (domain.MenuSyncResult, error)
	CreateOrder(context.Context, domain.BuildOrderInput, string, [32]byte) (domain.Order, bool, error)
	GetOrder(context.Context, string, string) (domain.Order, error)
	ListOrders(context.Context, string, int, int) ([]domain.Order, bool, error)
	ListIntegrationOrders(context.Context, string, *Cursor, int) ([]domain.Order, *Cursor, error)
	DecideOrder(context.Context, string, string, domain.OrderStatus, string, *string) (domain.Order, error)
	UpdateOrderStatus(context.Context, string, string, domain.OrderStatus) (domain.Order, error)
}

type Service struct {
	repository Repository
}

func New(repository Repository) *Service {
	return &Service{repository: repository}
}

func (s *Service) Ready(ctx context.Context) error {
	return s.repository.Ping(ctx)
}

func (s *Service) ListEstablishments(ctx context.Context, limit, offset int) ([]domain.Establishment, bool, error) {
	return s.repository.ListEstablishments(ctx, limit, offset)
}

func (s *Service) GetMenu(ctx context.Context, establishmentID string) (domain.Menu, error) {
	return s.repository.GetMenu(ctx, establishmentID)
}

func (s *Service) AuthenticateEstablishment(ctx context.Context, key string) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", domain.NewError(domain.CodeUnauthorized, "integration key is missing or invalid", nil)
	}
	return s.repository.AuthenticateEstablishment(ctx, key)
}

func (s *Service) SyncMenu(ctx context.Context, establishmentID string, items []domain.MenuSyncItem) (domain.MenuSyncResult, error) {
	if len(items) > 1000 {
		return domain.MenuSyncResult{}, domain.NewError(domain.CodeValidation, "menu cannot contain more than 1000 items", nil)
	}
	seen := make(map[string]struct{}, len(items))
	for i := range items {
		items[i].ExternalID = strings.TrimSpace(items[i].ExternalID)
		items[i].Name = strings.TrimSpace(items[i].Name)
		if items[i].ExternalID == "" || len(items[i].ExternalID) > 128 || items[i].Name == "" || len(items[i].Name) > 200 || items[i].PriceMinor < 1 {
			return domain.MenuSyncResult{}, domain.NewError(domain.CodeValidation, "invalid menu item", map[string]any{"external_id": items[i].ExternalID})
		}
		if _, exists := seen[items[i].ExternalID]; exists {
			return domain.MenuSyncResult{}, domain.NewError(domain.CodeValidation, "duplicate external menu item id", map[string]any{"external_id": items[i].ExternalID})
		}
		seen[items[i].ExternalID] = struct{}{}
	}
	return s.repository.SyncMenu(ctx, establishmentID, items)
}

func (s *Service) CreateOrder(ctx context.Context, input domain.BuildOrderInput, idempotencyKey string) (domain.Order, bool, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return domain.Order{}, false, domain.NewError(domain.CodeValidation, "invalid idempotency key", nil)
	}
	hash, err := orderRequestHash(input)
	if err != nil {
		return domain.Order{}, false, err
	}
	return s.repository.CreateOrder(ctx, input, idempotencyKey, hash)
}

func (s *Service) GetOrder(ctx context.Context, orderID, userID string) (domain.Order, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > 128 {
		return domain.Order{}, domain.NewError(domain.CodeValidation, "invalid user id", nil)
	}
	return s.repository.GetOrder(ctx, orderID, userID)
}

func (s *Service) ListOrders(ctx context.Context, userID string, limit, offset int) ([]domain.Order, bool, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" || len(userID) > 128 {
		return nil, false, domain.NewError(domain.CodeValidation, "invalid user id", nil)
	}
	return s.repository.ListOrders(ctx, userID, limit, offset)
}

func (s *Service) ListIntegrationOrders(ctx context.Context, establishmentID, encodedCursor string, limit int) ([]domain.Order, string, error) {
	var cursor *Cursor
	if encodedCursor != "" {
		decoded, err := decodeCursor(encodedCursor)
		if err != nil {
			return nil, "", domain.NewError(domain.CodeValidation, "invalid cursor", nil)
		}
		cursor = &decoded
	}
	orders, next, err := s.repository.ListIntegrationOrders(ctx, establishmentID, cursor, limit)
	if err != nil || next == nil {
		return orders, "", err
	}
	encoded, err := encodeCursor(*next)
	if err != nil {
		return nil, "", err
	}
	return orders, encoded, nil
}

func (s *Service) DecideOrder(ctx context.Context, establishmentID, orderID, decision, externalOrderID string, reason *string) (domain.Order, error) {
	externalOrderID = strings.TrimSpace(externalOrderID)
	if externalOrderID == "" || len(externalOrderID) > 128 {
		return domain.Order{}, domain.NewError(domain.CodeValidation, "invalid external order id", nil)
	}
	status := domain.OrderStatus(decision)
	if status != domain.StatusAccepted && status != domain.StatusRejected {
		return domain.Order{}, domain.NewError(domain.CodeValidation, "decision must be accepted or rejected", nil)
	}
	if status == domain.StatusRejected {
		if reason == nil || strings.TrimSpace(*reason) == "" || len(strings.TrimSpace(*reason)) > 500 {
			return domain.Order{}, domain.NewError(domain.CodeValidation, "rejection reason is required", nil)
		}
		trimmed := strings.TrimSpace(*reason)
		reason = &trimmed
	} else {
		reason = nil
	}
	return s.repository.DecideOrder(ctx, establishmentID, orderID, status, externalOrderID, reason)
}

func (s *Service) UpdateOrderStatus(ctx context.Context, establishmentID, orderID, status string) (domain.Order, error) {
	target := domain.OrderStatus(status)
	if target != domain.StatusPreparing && target != domain.StatusReady {
		return domain.Order{}, domain.NewError(domain.CodeValidation, "status must be preparing or ready", nil)
	}
	return s.repository.UpdateOrderStatus(ctx, establishmentID, orderID, target)
}

func orderRequestHash(input domain.BuildOrderInput) ([32]byte, error) {
	normalized := input
	normalized.UserID = strings.TrimSpace(normalized.UserID)
	normalized.DeliveryAddress = strings.TrimSpace(normalized.DeliveryAddress)
	normalized.Items = append([]domain.RequestedItem(nil), input.Items...)
	sort.Slice(normalized.Items, func(i, j int) bool {
		if normalized.Items[i].MenuItemID == normalized.Items[j].MenuItemID {
			return normalized.Items[i].Quantity < normalized.Items[j].Quantity
		}
		return normalized.Items[i].MenuItemID < normalized.Items[j].MenuItemID
	})
	payload, err := json.Marshal(normalized)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(payload), nil
}

func encodeCursor(cursor Cursor) (string, error) {
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload), nil
}

func decodeCursor(value string) (Cursor, error) {
	payload, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return Cursor{}, err
	}
	var cursor Cursor
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return Cursor{}, err
	}
	if cursor.UpdatedAt.IsZero() || cursor.OrderID == "" {
		return Cursor{}, domain.NewError(domain.CodeValidation, "invalid cursor", nil)
	}
	if _, err := uuid.Parse(cursor.OrderID); err != nil {
		return Cursor{}, domain.NewError(domain.CodeValidation, "invalid cursor", nil)
	}
	return cursor, nil
}
