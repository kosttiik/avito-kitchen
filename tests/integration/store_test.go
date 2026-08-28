//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/application"
	"github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/domain"
	postgresstore "github.com/talense-tasks/backend-trainee-assignment-autumn-2026-kosttiik-4a1f9845/internal/kitchen/postgres"
)

const demoEstablishmentID = "11111111-1111-4111-8111-111111111111"

func TestStoreBusinessFlow(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `TRUNCATE order_items, orders, menu_items`); err != nil {
		t.Fatal(err)
	}

	service := application.New(postgresstore.New(pool))
	establishmentID, err := service.AuthenticateEstablishment(ctx, "demo-establishment-key")
	if err != nil {
		t.Fatalf("authenticate establishment: %v", err)
	}
	if establishmentID != demoEstablishmentID {
		t.Fatalf("establishment id = %s", establishmentID)
	}
	if _, err := service.AuthenticateEstablishment(ctx, "wrong-key"); !hasCode(err, domain.CodeUnauthorized) {
		t.Fatalf("wrong key error = %v", err)
	}
	inactiveID := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO establishments (id, name, kind, is_active) VALUES ($1, 'Inactive', 'restaurant', false)`, inactiveID); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = pool.Exec(context.Background(), `DELETE FROM establishments WHERE id = $1`, inactiveID) }()
	establishments, _, err := service.ListEstablishments(ctx, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, establishment := range establishments {
		if establishment.ID == inactiveID {
			t.Fatal("inactive establishment is visible")
		}
	}

	syncResult, err := service.SyncMenu(ctx, establishmentID, []domain.MenuSyncItem{
		{ExternalID: "pizza", Name: "Pizza", Description: "Tomato and cheese", PriceMinor: 69000, Available: true},
		{ExternalID: "tea", Name: "Tea", Description: "Black tea", PriceMinor: 25000, Available: true},
	})
	if err != nil {
		t.Fatalf("sync menu: %v", err)
	}
	if syncResult.ActiveItems != 2 || syncResult.DeactivatedItems != 0 {
		t.Fatalf("unexpected sync result: %#v", syncResult)
	}

	menu, err := service.GetMenu(ctx, establishmentID)
	if err != nil {
		t.Fatalf("get menu: %v", err)
	}
	if len(menu.Items) != 2 {
		t.Fatalf("menu item count = %d", len(menu.Items))
	}
	menuByExternalID := make(map[string]domain.MenuItem, len(menu.Items))
	for _, item := range menu.Items {
		menuByExternalID[item.ExternalID] = item
	}

	input := domain.BuildOrderInput{
		EstablishmentID: establishmentID,
		UserID:          "integration-user",
		DeliveryAddress: "Moscow, Tverskaya 1",
		Items: []domain.RequestedItem{
			{MenuItemID: menuByExternalID["pizza"].ID, Quantity: 2, ExpectedPriceMinor: 69000},
			{MenuItemID: menuByExternalID["tea"].ID, Quantity: 1, ExpectedPriceMinor: 25000},
		},
	}
	order, replayed, err := service.CreateOrder(ctx, input, "integration-order-1")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if replayed || order.TotalPriceMinor != 163000 || order.Status != domain.StatusPending {
		t.Fatalf("unexpected created order: %#v replayed=%v", order, replayed)
	}
	replayedOrder, replayed, err := service.CreateOrder(ctx, input, "integration-order-1")
	if err != nil || !replayed || replayedOrder.ID != order.ID {
		t.Fatalf("idempotent replay: order=%#v replayed=%v err=%v", replayedOrder, replayed, err)
	}
	changedInput := input
	changedInput.DeliveryAddress = "Different address"
	if _, _, err := service.CreateOrder(ctx, changedInput, "integration-order-1"); !hasCode(err, domain.CodeIdempotencyKeyReused) {
		t.Fatalf("reused key error = %v", err)
	}

	if _, err := service.SyncMenu(ctx, establishmentID, []domain.MenuSyncItem{
		{ExternalID: "pizza", Name: "Renamed Pizza", Description: "New recipe", PriceMinor: 79000, Available: false},
	}); err != nil {
		t.Fatalf("update menu: %v", err)
	}
	menu, err = service.GetMenu(ctx, establishmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 1 || menu.Items[0].ExternalID != "pizza" || menu.Items[0].Available {
		t.Fatalf("full snapshot semantics failed: %#v", menu.Items)
	}
	stored, err := service.GetOrder(ctx, order.ID, input.UserID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Items) != 2 || stored.Items[0].Name != "Pizza" || stored.Items[0].UnitPriceMinor != 69000 || stored.Items[1].ExternalItemID != "tea" || stored.Items[1].UnitPriceMinor != 25000 {
		t.Fatalf("order snapshot changed: %#v", stored.Items)
	}
	staleInput := domain.BuildOrderInput{
		EstablishmentID: establishmentID,
		UserID:          "integration-user-2",
		DeliveryAddress: "Address",
		Items: []domain.RequestedItem{{
			MenuItemID: menu.Items[0].ID, Quantity: 1, ExpectedPriceMinor: 69000,
		}},
	}
	if _, _, err := service.CreateOrder(ctx, staleInput, "integration-order-2"); !hasCode(err, domain.CodeMenuItemUnavailable) {
		t.Fatalf("stale order error = %v", err)
	}

	orders, cursor, err := service.ListIntegrationOrders(ctx, establishmentID, "", 1)
	if err != nil || len(orders) != 1 || cursor == "" {
		t.Fatalf("integration orders: count=%d cursor=%q err=%v", len(orders), cursor, err)
	}
	if next, _, err := service.ListIntegrationOrders(ctx, establishmentID, cursor, 1); err != nil || len(next) != 0 {
		t.Fatalf("cursor replay returned %d orders: %v", len(next), err)
	}

	accepted, err := service.DecideOrder(ctx, establishmentID, order.ID, "accepted", "example-"+order.ID, nil)
	if err != nil || accepted.Status != domain.StatusAccepted {
		t.Fatalf("accept order: %#v %v", accepted, err)
	}
	for _, status := range []string{"preparing", "ready"} {
		accepted, err = service.UpdateOrderStatus(ctx, establishmentID, order.ID, status)
		if err != nil || string(accepted.Status) != status {
			t.Fatalf("update status %s: %#v %v", status, accepted, err)
		}
	}
	if _, err := service.UpdateOrderStatus(ctx, establishmentID, order.ID, "preparing"); !hasCode(err, domain.CodeInvalidOrderTransition) {
		t.Fatalf("invalid transition error = %v", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO menu_items (id, establishment_id, external_id, name, price_minor)
		VALUES ($1, $2, 'invalid-price', 'Invalid', -1)
	`, uuid.New(), establishmentID); err == nil {
		t.Fatal("negative menu price passed database constraint")
	}
}

func TestConcurrentOrderCreationIsIdempotent(t *testing.T) {
	ctx, pool, service := integrationService(t)
	if _, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{{
		ExternalID: "concurrent-pizza", Name: "Pizza", PriceMinor: 50000, Available: true,
	}}); err != nil {
		t.Fatal(err)
	}
	menu, err := service.GetMenu(ctx, demoEstablishmentID)
	if err != nil {
		t.Fatal(err)
	}
	input := domain.BuildOrderInput{
		EstablishmentID: demoEstablishmentID,
		UserID:          "concurrent-user",
		DeliveryAddress: "Moscow",
		Items: []domain.RequestedItem{{
			MenuItemID: menu.Items[0].ID, Quantity: 1, ExpectedPriceMinor: menu.Items[0].PriceMinor,
		}},
	}

	const attempts = 8
	start := make(chan struct{})
	results := make(chan createResult, attempts)
	var group sync.WaitGroup
	for range attempts {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			order, replayed, err := service.CreateOrder(ctx, input, "concurrent-key")
			results <- createResult{orderID: order.ID, replayed: replayed, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(results)

	created := 0
	orderID := ""
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent create: %v", result.err)
		}
		if !result.replayed {
			created++
		}
		if orderID == "" {
			orderID = result.orderID
		} else if result.orderID != orderID {
			t.Fatalf("different order ids: %s and %s", orderID, result.orderID)
		}
	}
	if created != 1 {
		t.Fatalf("created requests = %d, want 1", created)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE user_id = 'concurrent-user'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored orders = %d, want 1", count)
	}
}

func TestMenuSyncWaitsForOrderReadLock(t *testing.T) {
	ctx, pool, service := integrationService(t)
	if _, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{{
		ExternalID: "locked-pizza", Name: "Pizza", PriceMinor: 50000, Available: true,
	}}); err != nil {
		t.Fatal(err)
	}
	menu, err := service.GetMenu(ctx, demoEstablishmentID)
	if err != nil {
		t.Fatal(err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedPrice int64
	var lockedAvailable bool
	if err := tx.QueryRow(ctx, `
		SELECT price_minor, is_available
		FROM menu_items
		WHERE id = $1
		FOR SHARE
	`, menu.Items[0].ID).Scan(&lockedPrice, &lockedAvailable); err != nil {
		t.Fatal(err)
	}

	databaseURL := os.Getenv("TEST_DATABASE_URL")
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	applicationName := "menu-sync-lock-" + uuid.NewString()
	config.ConnConfig.RuntimeParams["application_name"] = applicationName
	config.MaxConns = 1
	syncPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer syncPool.Close()
	syncService := application.New(postgresstore.New(syncPool))
	syncResult := make(chan error, 1)
	go func() {
		_, syncErr := syncService.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{{
			ExternalID: "locked-pizza", Name: "Updated Pizza", PriceMinor: 60000, Available: false,
		}})
		syncResult <- syncErr
	}()

	waitForDatabaseLock(t, ctx, pool, applicationName)
	var visiblePrice int64
	var visibleAvailable bool
	if err := pool.QueryRow(ctx, `SELECT price_minor, is_available FROM menu_items WHERE id = $1`, menu.Items[0].ID).Scan(&visiblePrice, &visibleAvailable); err != nil {
		t.Fatal(err)
	}
	if visiblePrice != lockedPrice || visibleAvailable != lockedAvailable {
		t.Fatalf("visible menu changed while row was locked: price=%d available=%v", visiblePrice, visibleAvailable)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-syncResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := pool.QueryRow(ctx, `SELECT price_minor, is_available FROM menu_items WHERE id = $1`, menu.Items[0].ID).Scan(&visiblePrice, &visibleAvailable); err != nil {
		t.Fatal(err)
	}
	if visiblePrice != 60000 || visibleAvailable {
		t.Fatalf("menu sync was not applied: price=%d available=%v", visiblePrice, visibleAvailable)
	}
}

func TestIntegrationOwnershipAndCursorTieBreaking(t *testing.T) {
	ctx, pool, service := integrationService(t)
	if _, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{{
		ExternalID: "cursor-item", Name: "Item", PriceMinor: 10000, Available: true,
	}}); err != nil {
		t.Fatal(err)
	}
	menu, err := service.GetMenu(ctx, demoEstablishmentID)
	if err != nil {
		t.Fatal(err)
	}
	for i, userID := range []string{"cursor-user-1", "cursor-user-2"} {
		input := domain.BuildOrderInput{
			EstablishmentID: demoEstablishmentID,
			UserID:          userID,
			DeliveryAddress: "Moscow",
			Items: []domain.RequestedItem{{
				MenuItemID: menu.Items[0].ID, Quantity: 1, ExpectedPriceMinor: 10000,
			}},
		}
		if _, _, err := service.CreateOrder(ctx, input, "cursor-key-"+string(rune('1'+i))); err != nil {
			t.Fatal(err)
		}
	}
	fixedTime := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `UPDATE orders SET updated_at = $1`, fixedTime); err != nil {
		t.Fatal(err)
	}

	first, cursor, err := service.ListIntegrationOrders(ctx, demoEstablishmentID, "", 1)
	if err != nil || len(first) != 1 || cursor == "" {
		t.Fatalf("first cursor page: count=%d cursor=%q err=%v", len(first), cursor, err)
	}
	second, nextCursor, err := service.ListIntegrationOrders(ctx, demoEstablishmentID, cursor, 1)
	if err != nil || len(second) != 1 || nextCursor == "" || second[0].ID == first[0].ID {
		t.Fatalf("second cursor page: orders=%#v cursor=%q err=%v", second, nextCursor, err)
	}
	tail, _, err := service.ListIntegrationOrders(ctx, demoEstablishmentID, nextCursor, 1)
	if err != nil || len(tail) != 0 {
		t.Fatalf("cursor tail: count=%d err=%v", len(tail), err)
	}

	otherID := uuid.NewString()
	otherKey := "other-establishment-key-" + uuid.NewString()
	otherHash := sha256.Sum256([]byte(otherKey))
	if _, err := pool.Exec(ctx, `INSERT INTO establishments (id, name, kind) VALUES ($1, 'Other', 'restaurant')`, otherID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO establishment_integrations (establishment_id, external_id, api_key_hash)
		VALUES ($1, $2, $3)
	`, otherID, "other-"+otherID, otherHash[:]); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM establishment_integrations WHERE establishment_id = $1`, otherID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM establishments WHERE id = $1`, otherID)
	})
	authenticatedOtherID, err := service.AuthenticateEstablishment(ctx, otherKey)
	if err != nil || authenticatedOtherID != otherID {
		t.Fatalf("other establishment auth: id=%q err=%v", authenticatedOtherID, err)
	}
	otherOrders, _, err := service.ListIntegrationOrders(ctx, otherID, "", 10)
	if err != nil || len(otherOrders) != 0 {
		t.Fatalf("other establishment saw orders: count=%d err=%v", len(otherOrders), err)
	}
	if _, err := service.DecideOrder(ctx, otherID, first[0].ID, "accepted", "foreign-order", nil); !hasCode(err, domain.CodeNotFound) {
		t.Fatalf("cross-establishment decision error = %v", err)
	}
}

func TestIntegrationCursorDoesNotSkipConcurrentUpdate(t *testing.T) {
	ctx, pool, service := integrationService(t)
	menuItem := syncTestMenuItem(t, ctx, service, "cursor-race-item")
	orders := make([]domain.Order, 0, 3)
	for i := range 3 {
		orders = append(orders, createTestOrder(t, ctx, service, menuItem, "cursor-race-user-"+string(rune('1'+i)), "cursor-race-key-"+string(rune('1'+i))))
	}
	base := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	for i, order := range orders {
		at := base.Add(time.Duration(i) * time.Minute)
		if _, err := pool.Exec(ctx, `UPDATE orders SET created_at = $2, updated_at = $2 WHERE id = $1`, order.ID, at); err != nil {
			t.Fatal(err)
		}
	}

	barrier := &queryBarrierTracer{reached: make(chan struct{}), release: make(chan struct{})}
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.Tracer = barrier
	config.MaxConns = 1
	pollPool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pollPool.Close)
	pollService := application.New(postgresstore.New(pollPool))

	type pageResult struct {
		orders []domain.Order
		cursor string
		err    error
	}
	page := make(chan pageResult, 1)
	go func() {
		listed, cursor, listErr := pollService.ListIntegrationOrders(ctx, demoEstablishmentID, "", 2)
		page <- pageResult{orders: listed, cursor: cursor, err: listErr}
	}()

	select {
	case <-barrier.reached:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET updated_at = $2 WHERE id = $1`, orders[1].ID, base.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	close(barrier.release)

	first := <-page
	if first.err != nil || len(first.orders) != 2 || first.cursor == "" {
		t.Fatalf("first page: count=%d cursor=%q err=%v", len(first.orders), first.cursor, first.err)
	}
	next, _, err := pollService.ListIntegrationOrders(ctx, demoEstablishmentID, first.cursor, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !containsOrder(next, orders[2].ID) {
		t.Fatalf("unread order %s was skipped after concurrent update: %#v", orders[2].ID, next)
	}
}

func TestListOrdersPaginationAndOwnership(t *testing.T) {
	ctx, pool, service := integrationService(t)
	menuItem := syncTestMenuItem(t, ctx, service, "history-item")
	userOrders := make([]domain.Order, 0, 3)
	for i := range 3 {
		userOrders = append(userOrders, createTestOrder(t, ctx, service, menuItem, "history-user", "history-key-"+string(rune('1'+i))))
	}
	other := createTestOrder(t, ctx, service, menuItem, "other-history-user", "other-history-key")
	base := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	for i, order := range userOrders {
		at := base.Add(time.Duration(i) * time.Minute)
		if _, err := pool.Exec(ctx, `UPDATE orders SET created_at = $2, updated_at = $2 WHERE id = $1`, order.ID, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE orders SET created_at = $2, updated_at = $2 WHERE id = $1`, other.ID, base.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	first, hasMore, err := service.ListOrders(ctx, "history-user", 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !hasMore || len(first) != 2 || first[0].ID != userOrders[2].ID || first[1].ID != userOrders[1].ID {
		t.Fatalf("first history page: orders=%#v hasMore=%v", first, hasMore)
	}
	second, hasMore, err := service.ListOrders(ctx, "history-user", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if hasMore || len(second) != 1 || second[0].ID != userOrders[0].ID {
		t.Fatalf("second history page: orders=%#v hasMore=%v", second, hasMore)
	}
	for _, order := range append(first, second...) {
		if order.UserID != "history-user" || order.ID == other.ID {
			t.Fatalf("history ownership violated: %#v", order)
		}
	}
}

func TestEmptyMenuSnapshotDeactivatesAllItems(t *testing.T) {
	ctx, pool, service := integrationService(t)
	if _, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{
		{ExternalID: "empty-one", Name: "One", PriceMinor: 10000, Available: true},
		{ExternalID: "empty-two", Name: "Two", PriceMinor: 20000, Available: true},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{})
	if err != nil {
		t.Fatal(err)
	}
	if result.ActiveItems != 0 || result.DeactivatedItems != 2 {
		t.Fatalf("empty snapshot result = %#v", result)
	}
	menu, err := service.GetMenu(ctx, demoEstablishmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 0 {
		t.Fatalf("public menu still contains items: %#v", menu.Items)
	}
	var active, available int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE is_active), count(*) FILTER (WHERE is_available)
		FROM menu_items WHERE establishment_id = $1
	`, demoEstablishmentID).Scan(&active, &available); err != nil {
		t.Fatal(err)
	}
	if active != 0 || available != 0 {
		t.Fatalf("stored active=%d available=%d", active, available)
	}
}

func TestDecisionReplayAndExternalOrderIDCollision(t *testing.T) {
	ctx, _, service := integrationService(t)
	menuItem := syncTestMenuItem(t, ctx, service, "decision-item")
	first := createTestOrder(t, ctx, service, menuItem, "decision-user-1", "decision-key-1")
	second := createTestOrder(t, ctx, service, menuItem, "decision-user-2", "decision-key-2")
	third := createTestOrder(t, ctx, service, menuItem, "decision-user-3", "decision-key-3")

	accepted, err := service.DecideOrder(ctx, demoEstablishmentID, first.ID, "accepted", "shared-external-id", nil)
	if err != nil || accepted.Status != domain.StatusAccepted {
		t.Fatalf("accept first order: %#v %v", accepted, err)
	}
	replayed, err := service.DecideOrder(ctx, demoEstablishmentID, first.ID, "accepted", "shared-external-id", nil)
	if err != nil || replayed.ID != first.ID || replayed.Status != domain.StatusAccepted {
		t.Fatalf("exact decision replay: %#v %v", replayed, err)
	}
	if _, err := service.DecideOrder(ctx, demoEstablishmentID, first.ID, "accepted", "different-external-id", nil); !hasCode(err, domain.CodeConflict) {
		t.Fatalf("mismatched replay error = %v", err)
	}
	if _, err := service.DecideOrder(ctx, demoEstablishmentID, second.ID, "accepted", "shared-external-id", nil); !hasCode(err, domain.CodeConflict) {
		t.Fatalf("external id collision error = %v", err)
	}

	reason := "out_of_stock"
	rejected, err := service.DecideOrder(ctx, demoEstablishmentID, third.ID, "rejected", "rejected-external-id", &reason)
	if err != nil || rejected.Status != domain.StatusRejected {
		t.Fatalf("reject order: %#v %v", rejected, err)
	}
	if _, err := service.DecideOrder(ctx, demoEstablishmentID, third.ID, "rejected", "rejected-external-id", &reason); err != nil {
		t.Fatalf("exact rejection replay: %v", err)
	}
	changedReason := "closed"
	if _, err := service.DecideOrder(ctx, demoEstablishmentID, third.ID, "rejected", "rejected-external-id", &changedReason); !hasCode(err, domain.CodeConflict) {
		t.Fatalf("mismatched rejection replay error = %v", err)
	}
}

func TestDatabaseConstraints(t *testing.T) {
	ctx, pool, _ := integrationService(t)
	requestHash := sha256.Sum256([]byte("constraint-test"))
	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "negative menu price",
			query: `INSERT INTO menu_items (id, establishment_id, external_id, name, price_minor) VALUES ($1, $2, $3, 'Invalid', -1)`,
			args:  []any{uuid.New(), demoEstablishmentID, "negative-" + uuid.NewString()},
		},
		{
			name:  "broken establishment foreign key",
			query: `INSERT INTO menu_items (id, establishment_id, external_id, name, price_minor) VALUES ($1, $2, $3, 'Invalid', 1)`,
			args:  []any{uuid.New(), uuid.New(), "foreign-" + uuid.NewString()},
		},
		{
			name: "unknown order status",
			query: `
				INSERT INTO orders (
					id, establishment_id, user_id, delivery_address, status,
					total_price_minor, idempotency_key, request_hash
				) VALUES ($1, $2, 'constraint-user', 'Moscow', 'unknown', 1, $3, $4)
			`,
			args: []any{uuid.New(), demoEstablishmentID, "status-" + uuid.NewString(), requestHash[:]},
		},
		{
			name: "rejection without reason",
			query: `
				INSERT INTO orders (
					id, establishment_id, user_id, delivery_address, status,
					total_price_minor, idempotency_key, request_hash
				) VALUES ($1, $2, 'constraint-user', 'Moscow', 'rejected', 1, $3, $4)
			`,
			args: []any{uuid.New(), demoEstablishmentID, "reason-" + uuid.NewString(), requestHash[:]},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, test.query, test.args...); err == nil {
				t.Fatal("database accepted invalid row")
			}
		})
	}
}

type createResult struct {
	orderID  string
	replayed bool
	err      error
}

type queryBarrierTracer struct {
	once    sync.Once
	reached chan struct{}
	release chan struct{}
}

func (t *queryBarrierTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "SELECT id, establishment_id, user_id, delivery_address") {
		t.once.Do(func() {
			close(t.reached)
			select {
			case <-t.release:
			case <-ctx.Done():
			}
		})
	}
	return ctx
}

func (*queryBarrierTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func syncTestMenuItem(t *testing.T, ctx context.Context, service *application.Service, externalID string) domain.MenuItem {
	t.Helper()
	if _, err := service.SyncMenu(ctx, demoEstablishmentID, []domain.MenuSyncItem{{
		ExternalID: externalID,
		Name:       "Test item",
		PriceMinor: 10000,
		Available:  true,
	}}); err != nil {
		t.Fatal(err)
	}
	menu, err := service.GetMenu(ctx, demoEstablishmentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(menu.Items) != 1 {
		t.Fatalf("menu item count = %d", len(menu.Items))
	}
	return menu.Items[0]
}

func createTestOrder(t *testing.T, ctx context.Context, service *application.Service, item domain.MenuItem, userID, key string) domain.Order {
	t.Helper()
	order, replayed, err := service.CreateOrder(ctx, domain.BuildOrderInput{
		EstablishmentID: demoEstablishmentID,
		UserID:          userID,
		DeliveryAddress: "Moscow",
		Items: []domain.RequestedItem{{
			MenuItemID: item.ID, Quantity: 1, ExpectedPriceMinor: item.PriceMinor,
		}},
	}, key)
	if err != nil || replayed {
		t.Fatalf("create test order: replayed=%v err=%v", replayed, err)
	}
	return order
}

func containsOrder(orders []domain.Order, orderID string) bool {
	for _, order := range orders {
		if order.ID == orderID {
			return true
		}
	}
	return false
}

func integrationService(t *testing.T) (context.Context, *pgxpool.Pool, *application.Service) {
	t.Helper()
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE order_items, orders, menu_items`); err != nil {
		t.Fatal(err)
	}
	return ctx, pool, application.New(postgresstore.New(pool))
}

func waitForDatabaseLock(t *testing.T, ctx context.Context, pool *pgxpool.Pool, applicationName string) {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_stat_activity
				WHERE application_name = $1 AND wait_event_type = 'Lock'
			)
		`, applicationName).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

func hasCode(err error, code domain.Code) bool {
	var domainErr *domain.Error
	return errors.As(err, &domainErr) && domainErr.Code == code
}
