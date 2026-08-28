package postgres

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/application"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/domain"
)

type Store struct {
	pool *pgxpool.Pool
}

type dbtx interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) ListEstablishments(ctx context.Context, limit, offset int) ([]domain.Establishment, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name, kind, description
		FROM establishments
		WHERE is_active
		ORDER BY name, id
		LIMIT $1 OFFSET $2
	`, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("list establishments: %w", err)
	}
	defer rows.Close()

	items := make([]domain.Establishment, 0, limit+1)
	for rows.Next() {
		var item domain.Establishment
		if err := rows.Scan(&item.ID, &item.Name, &item.Kind, &item.Description); err != nil {
			return nil, false, fmt.Errorf("scan establishment: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("iterate establishments: %w", err)
	}
	hasMore := len(items) > limit
	if hasMore {
		items = items[:limit]
	}
	return items, hasMore, nil
}

func (s *Store) GetMenu(ctx context.Context, establishmentID string) (domain.Menu, error) {
	var menu domain.Menu
	err := s.pool.QueryRow(ctx, `
		SELECT id, name, kind, description
		FROM establishments
		WHERE id = $1 AND is_active
	`, establishmentID).Scan(
		&menu.Establishment.ID,
		&menu.Establishment.Name,
		&menu.Establishment.Kind,
		&menu.Establishment.Description,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Menu{}, domain.NewError(domain.CodeNotFound, "establishment not found", nil)
	}
	if err != nil {
		return domain.Menu{}, fmt.Errorf("get establishment menu owner: %w", err)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, establishment_id, external_id, name, description, price_minor,
		       is_active, is_available, updated_at
		FROM menu_items
		WHERE establishment_id = $1 AND is_active
		ORDER BY name, id
	`, establishmentID)
	if err != nil {
		return domain.Menu{}, fmt.Errorf("list menu items: %w", err)
	}
	defer rows.Close()

	menu.Items = make([]domain.MenuItem, 0)
	for rows.Next() {
		var item domain.MenuItem
		if err := rows.Scan(
			&item.ID,
			&item.EstablishmentID,
			&item.ExternalID,
			&item.Name,
			&item.Description,
			&item.PriceMinor,
			&item.Active,
			&item.Available,
			&item.UpdatedAt,
		); err != nil {
			return domain.Menu{}, fmt.Errorf("scan menu item: %w", err)
		}
		menu.Items = append(menu.Items, item)
	}
	if err := rows.Err(); err != nil {
		return domain.Menu{}, fmt.Errorf("iterate menu items: %w", err)
	}
	return menu, nil
}

func (s *Store) AuthenticateEstablishment(ctx context.Context, key string) (string, error) {
	hash := sha256.Sum256([]byte(key))
	var establishmentID string
	err := s.pool.QueryRow(ctx, `
		SELECT i.establishment_id
		FROM establishment_integrations i
		JOIN establishments e ON e.id = i.establishment_id
		WHERE i.api_key_hash = $1 AND i.is_active AND e.is_active
	`, hash[:]).Scan(&establishmentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", domain.NewError(domain.CodeUnauthorized, "integration key is missing or invalid", nil)
	}
	if err != nil {
		return "", fmt.Errorf("authenticate establishment: %w", err)
	}
	return establishmentID, nil
}

func (s *Store) SyncMenu(ctx context.Context, establishmentID string, items []domain.MenuSyncItem) (domain.MenuSyncResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.MenuSyncResult{}, fmt.Errorf("begin menu sync: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var syncedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&syncedAt); err != nil {
		return domain.MenuSyncResult{}, fmt.Errorf("read menu sync time: %w", err)
	}

	externalIDs := make([]string, 0, len(items))
	for _, item := range items {
		externalIDs = append(externalIDs, item.ExternalID)
		_, err := tx.Exec(ctx, `
			INSERT INTO menu_items (
				establishment_id, external_id, name, description, price_minor,
				currency, is_active, is_available, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, 'RUB', true, $6, $7)
			ON CONFLICT (establishment_id, external_id) DO UPDATE
			SET name = EXCLUDED.name,
			    description = EXCLUDED.description,
			    price_minor = EXCLUDED.price_minor,
			    is_active = true,
			    is_available = EXCLUDED.is_available,
			    updated_at = EXCLUDED.updated_at
		`, establishmentID, item.ExternalID, item.Name, item.Description, item.PriceMinor, item.Available, syncedAt)
		if err != nil {
			return domain.MenuSyncResult{}, fmt.Errorf("upsert menu item %q: %w", item.ExternalID, err)
		}
	}

	var tag pgconn.CommandTag
	if len(externalIDs) == 0 {
		tag, err = tx.Exec(ctx, `
			UPDATE menu_items
			SET is_active = false, is_available = false, updated_at = $2
			WHERE establishment_id = $1 AND is_active
		`, establishmentID, syncedAt)
	} else {
		tag, err = tx.Exec(ctx, `
			UPDATE menu_items
			SET is_active = false, is_available = false, updated_at = $3
			WHERE establishment_id = $1
			  AND is_active
			  AND NOT (external_id = ANY($2::text[]))
		`, establishmentID, externalIDs, syncedAt)
	}
	if err != nil {
		return domain.MenuSyncResult{}, fmt.Errorf("deactivate omitted menu items: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.MenuSyncResult{}, fmt.Errorf("commit menu sync: %w", err)
	}
	return domain.MenuSyncResult{
		ActiveItems:      len(items),
		DeactivatedItems: int(tag.RowsAffected()),
		SyncedAt:         syncedAt,
	}, nil
}

func (s *Store) CreateOrder(ctx context.Context, input domain.BuildOrderInput, idempotencyKey string, requestHash [32]byte) (domain.Order, bool, error) {
	if existing, found, err := s.findIdempotentOrder(ctx, input.UserID, idempotencyKey, requestHash); err != nil || found {
		return existing, found, err
	}

	menuIDs := make([]uuid.UUID, 0, len(input.Items))
	for _, item := range input.Items {
		id, err := uuid.Parse(item.MenuItemID)
		if err != nil {
			return domain.Order{}, false, domain.NewError(domain.CodeValidation, "invalid menu item id", map[string]any{"menu_item_id": item.MenuItemID})
		}
		menuIDs = append(menuIDs, id)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("begin create order: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var establishmentExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM establishments WHERE id = $1 AND is_active)`, input.EstablishmentID).Scan(&establishmentExists); err != nil {
		return domain.Order{}, false, fmt.Errorf("check establishment: %w", err)
	}
	if !establishmentExists {
		return domain.Order{}, false, domain.NewError(domain.CodeNotFound, "establishment not found", nil)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, establishment_id, external_id, name, description, price_minor,
		       is_active, is_available, updated_at
		FROM menu_items
		WHERE id = ANY($1::uuid[])
		ORDER BY id
		FOR SHARE
	`, menuIDs)
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("lock order menu items: %w", err)
	}
	menuItems := make([]domain.MenuItem, 0, len(menuIDs))
	for rows.Next() {
		var item domain.MenuItem
		if err := rows.Scan(
			&item.ID,
			&item.EstablishmentID,
			&item.ExternalID,
			&item.Name,
			&item.Description,
			&item.PriceMinor,
			&item.Active,
			&item.Available,
			&item.UpdatedAt,
		); err != nil {
			rows.Close()
			return domain.Order{}, false, fmt.Errorf("scan locked menu item: %w", err)
		}
		menuItems = append(menuItems, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return domain.Order{}, false, fmt.Errorf("iterate locked menu items: %w", err)
	}
	rows.Close()

	draft, err := domain.BuildOrder(input, menuItems)
	if err != nil {
		return domain.Order{}, false, err
	}

	var order domain.Order
	err = tx.QueryRow(ctx, `
		INSERT INTO orders (
			establishment_id, user_id, delivery_address, status,
			total_price_minor, currency, idempotency_key, request_hash
		)
		VALUES ($1, $2, $3, 'pending', $4, 'RUB', $5, $6)
		RETURNING id, establishment_id, user_id, delivery_address, external_order_id,
		          status, rejection_reason, total_price_minor, created_at, updated_at
	`, draft.EstablishmentID, draft.UserID, draft.DeliveryAddress, draft.TotalPriceMinor, idempotencyKey, requestHash[:]).Scan(
		&order.ID,
		&order.EstablishmentID,
		&order.UserID,
		&order.DeliveryAddress,
		&order.ExternalOrderID,
		&order.Status,
		&order.RejectionReason,
		&order.TotalPriceMinor,
		&order.CreatedAt,
		&order.UpdatedAt,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			_ = tx.Rollback(ctx)
			existing, found, findErr := s.findIdempotentOrder(ctx, input.UserID, idempotencyKey, requestHash)
			if findErr != nil || found {
				return existing, found, findErr
			}
		}
		return domain.Order{}, false, fmt.Errorf("insert order: %w", err)
	}

	order.Items = append([]domain.OrderItem(nil), draft.Items...)
	for i, item := range order.Items {
		_, err := tx.Exec(ctx, `
			INSERT INTO order_items (
				order_id, line_no, menu_item_id, external_item_id, name,
				unit_price_minor, quantity, line_total_minor
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`, order.ID, i+1, item.MenuItemID, item.ExternalItemID, item.Name, item.UnitPriceMinor, item.Quantity, item.LineTotalMinor)
		if err != nil {
			return domain.Order{}, false, fmt.Errorf("insert order item: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, false, fmt.Errorf("commit create order: %w", err)
	}
	return order, false, nil
}

func (s *Store) GetOrder(ctx context.Context, orderID, userID string) (domain.Order, error) {
	return loadOrder(ctx, s.pool, `id = $1 AND user_id = $2`, orderID, userID)
}

func (s *Store) ListOrders(ctx context.Context, userID string, limit, offset int) ([]domain.Order, bool, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id
		FROM orders
		WHERE user_id = $1
		ORDER BY created_at DESC, id DESC
		LIMIT $2 OFFSET $3
	`, userID, limit+1, offset)
	if err != nil {
		return nil, false, fmt.Errorf("list user order ids: %w", err)
	}
	ids, err := collectIDs(rows)
	if err != nil {
		return nil, false, err
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	orders := make([]domain.Order, 0, len(ids))
	for _, id := range ids {
		order, err := loadOrder(ctx, s.pool, `id = $1 AND user_id = $2`, id, userID)
		if err != nil {
			return nil, false, err
		}
		orders = append(orders, order)
	}
	return orders, hasMore, nil
}

func (s *Store) ListIntegrationOrders(ctx context.Context, establishmentID string, cursor *application.Cursor, limit int) ([]domain.Order, *application.Cursor, error) {
	query := `
		SELECT id, updated_at
		FROM orders
		WHERE establishment_id = $1
		ORDER BY updated_at, id
		LIMIT $2
	`
	args := []any{establishmentID, limit + 1}
	if cursor != nil {
		query = `
			SELECT id, updated_at
			FROM orders
			WHERE establishment_id = $1
			  AND (updated_at, id) > ($2, $3::uuid)
			ORDER BY updated_at, id
			LIMIT $4
		`
		args = []any{establishmentID, cursor.UpdatedAt, cursor.OrderID, limit + 1}
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list integration order ids: %w", err)
	}
	positions, err := collectOrderPositions(rows)
	if err != nil {
		return nil, nil, err
	}
	if len(positions) > limit {
		positions = positions[:limit]
	}
	orders := make([]domain.Order, 0, len(positions))
	for _, position := range positions {
		order, err := loadOrder(ctx, s.pool, `id = $1 AND establishment_id = $2`, position.OrderID, establishmentID)
		if err != nil {
			return nil, nil, err
		}
		orders = append(orders, order)
	}
	if len(orders) == 0 {
		return orders, nil, nil
	}
	last := positions[len(positions)-1]
	return orders, &application.Cursor{UpdatedAt: last.UpdatedAt, OrderID: last.OrderID}, nil
}

func (s *Store) DecideOrder(ctx context.Context, establishmentID, orderID string, target domain.OrderStatus, externalOrderID string, reason *string) (domain.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("begin order decision: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, currentExternalID, currentReason, err := lockOrderState(ctx, tx, orderID, establishmentID)
	if err != nil {
		return domain.Order{}, err
	}
	if current == target {
		if currentExternalID == nil || *currentExternalID != externalOrderID || !equalOptionalString(currentReason, reason) {
			return domain.Order{}, domain.NewError(domain.CodeConflict, "decision replay does not match the existing order decision", nil)
		}
		if err := tx.Commit(ctx); err != nil {
			return domain.Order{}, fmt.Errorf("commit decision replay: %w", err)
		}
		return loadOrder(ctx, s.pool, `id = $1 AND establishment_id = $2`, orderID, establishmentID)
	}
	if err := domain.ValidateTransition(current, target); err != nil {
		return domain.Order{}, err
	}
	_, err = tx.Exec(ctx, `
		UPDATE orders
		SET status = $3, external_order_id = $4, rejection_reason = $5, updated_at = now()
		WHERE id = $1 AND establishment_id = $2
	`, orderID, establishmentID, target, externalOrderID, reason)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return domain.Order{}, domain.NewError(domain.CodeConflict, "external order id is already in use", nil)
		}
		return domain.Order{}, fmt.Errorf("update order decision: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, fmt.Errorf("commit order decision: %w", err)
	}
	return loadOrder(ctx, s.pool, `id = $1 AND establishment_id = $2`, orderID, establishmentID)
}

func (s *Store) UpdateOrderStatus(ctx context.Context, establishmentID, orderID string, target domain.OrderStatus) (domain.Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return domain.Order{}, fmt.Errorf("begin status update: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	current, _, _, err := lockOrderState(ctx, tx, orderID, establishmentID)
	if err != nil {
		return domain.Order{}, err
	}
	if err := domain.ValidateTransition(current, target); err != nil {
		return domain.Order{}, err
	}
	if current != target {
		if _, err := tx.Exec(ctx, `
			UPDATE orders SET status = $3, updated_at = now()
			WHERE id = $1 AND establishment_id = $2
		`, orderID, establishmentID, target); err != nil {
			return domain.Order{}, fmt.Errorf("update order status: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.Order{}, fmt.Errorf("commit order status: %w", err)
	}
	return loadOrder(ctx, s.pool, `id = $1 AND establishment_id = $2`, orderID, establishmentID)
}

func (s *Store) findIdempotentOrder(ctx context.Context, userID, idempotencyKey string, requestHash [32]byte) (domain.Order, bool, error) {
	var orderID string
	var storedHash []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id, request_hash
		FROM orders
		WHERE user_id = $1 AND idempotency_key = $2
	`, strings.TrimSpace(userID), idempotencyKey).Scan(&orderID, &storedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, false, nil
	}
	if err != nil {
		return domain.Order{}, false, fmt.Errorf("find idempotent order: %w", err)
	}
	if !bytes.Equal(storedHash, requestHash[:]) {
		return domain.Order{}, false, domain.NewError(domain.CodeIdempotencyKeyReused, "idempotency key was already used for another request", nil)
	}
	order, err := loadOrder(ctx, s.pool, `id = $1 AND user_id = $2`, orderID, strings.TrimSpace(userID))
	if err != nil {
		return domain.Order{}, false, err
	}
	return order, true, nil
}

func lockOrderState(ctx context.Context, tx pgx.Tx, orderID, establishmentID string) (domain.OrderStatus, *string, *string, error) {
	var status domain.OrderStatus
	var externalOrderID *string
	var reason *string
	err := tx.QueryRow(ctx, `
		SELECT status, external_order_id, rejection_reason
		FROM orders
		WHERE id = $1 AND establishment_id = $2
		FOR UPDATE
	`, orderID, establishmentID).Scan(&status, &externalOrderID, &reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, nil, domain.NewError(domain.CodeNotFound, "order not found", nil)
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf("lock order state: %w", err)
	}
	return status, externalOrderID, reason, nil
}

func loadOrder(ctx context.Context, q dbtx, predicate string, args ...any) (domain.Order, error) {
	query := `
		SELECT id, establishment_id, user_id, delivery_address, external_order_id,
		       status, rejection_reason, total_price_minor, created_at, updated_at
		FROM orders
		WHERE ` + predicate
	var order domain.Order
	err := q.QueryRow(ctx, query, args...).Scan(
		&order.ID,
		&order.EstablishmentID,
		&order.UserID,
		&order.DeliveryAddress,
		&order.ExternalOrderID,
		&order.Status,
		&order.RejectionReason,
		&order.TotalPriceMinor,
		&order.CreatedAt,
		&order.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Order{}, domain.NewError(domain.CodeNotFound, "order not found", nil)
	}
	if err != nil {
		return domain.Order{}, fmt.Errorf("load order: %w", err)
	}
	rows, err := q.Query(ctx, `
		SELECT menu_item_id, external_item_id, name, unit_price_minor, quantity, line_total_minor
		FROM order_items
		WHERE order_id = $1
		ORDER BY line_no
	`, order.ID)
	if err != nil {
		return domain.Order{}, fmt.Errorf("load order items: %w", err)
	}
	defer rows.Close()
	order.Items = make([]domain.OrderItem, 0)
	for rows.Next() {
		var item domain.OrderItem
		if err := rows.Scan(
			&item.MenuItemID,
			&item.ExternalItemID,
			&item.Name,
			&item.UnitPriceMinor,
			&item.Quantity,
			&item.LineTotalMinor,
		); err != nil {
			return domain.Order{}, fmt.Errorf("scan order item: %w", err)
		}
		order.Items = append(order.Items, item)
	}
	if err := rows.Err(); err != nil {
		return domain.Order{}, fmt.Errorf("iterate order items: %w", err)
	}
	return order, nil
}

func collectIDs(rows pgx.Rows) ([]string, error) {
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate ids: %w", err)
	}
	return ids, nil
}

type orderPosition struct {
	OrderID   string
	UpdatedAt time.Time
}

func collectOrderPositions(rows pgx.Rows) ([]orderPosition, error) {
	defer rows.Close()
	positions := make([]orderPosition, 0)
	for rows.Next() {
		var position orderPosition
		if err := rows.Scan(&position.OrderID, &position.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan order position: %w", err)
		}
		positions = append(positions, position)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate order positions: %w", err)
	}
	return positions, nil
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
