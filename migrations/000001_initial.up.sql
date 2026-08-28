CREATE TABLE establishments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    kind text NOT NULL CHECK (kind IN ('restaurant', 'cafe', 'store')),
    description text NOT NULL DEFAULT '',
    is_active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE establishment_integrations (
    establishment_id uuid PRIMARY KEY REFERENCES establishments(id) ON DELETE RESTRICT,
    external_id text NOT NULL UNIQUE CHECK (length(btrim(external_id)) BETWEEN 1 AND 128),
    api_key_hash bytea NOT NULL UNIQUE CHECK (octet_length(api_key_hash) = 32),
    is_active boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE menu_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    establishment_id uuid NOT NULL REFERENCES establishments(id) ON DELETE RESTRICT,
    external_id text NOT NULL CHECK (length(btrim(external_id)) BETWEEN 1 AND 128),
    name text NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 200),
    description text NOT NULL DEFAULT '',
    price_minor bigint NOT NULL CHECK (price_minor > 0),
    currency character(3) NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB'),
    is_active boolean NOT NULL DEFAULT true,
    is_available boolean NOT NULL DEFAULT true,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (establishment_id, external_id)
);

CREATE TABLE orders (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    establishment_id uuid NOT NULL REFERENCES establishments(id) ON DELETE RESTRICT,
    user_id text NOT NULL CHECK (length(btrim(user_id)) BETWEEN 1 AND 128),
    delivery_address text NOT NULL CHECK (length(btrim(delivery_address)) BETWEEN 1 AND 500),
    external_order_id text,
    status text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'preparing', 'ready', 'rejected')),
    rejection_reason text,
    total_price_minor bigint NOT NULL CHECK (total_price_minor > 0),
    currency character(3) NOT NULL DEFAULT 'RUB' CHECK (currency = 'RUB'),
    idempotency_key text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 128),
    request_hash bytea NOT NULL CHECK (octet_length(request_hash) = 32),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (user_id, idempotency_key),
    CHECK (
        (status = 'rejected' AND rejection_reason IS NOT NULL AND length(btrim(rejection_reason)) > 0)
        OR (status <> 'rejected' AND rejection_reason IS NULL)
    )
);

CREATE UNIQUE INDEX orders_establishment_external_id_uq
    ON orders (establishment_id, external_order_id)
    WHERE external_order_id IS NOT NULL;

CREATE TABLE order_items (
    order_id uuid NOT NULL REFERENCES orders(id) ON DELETE RESTRICT,
    line_no smallint NOT NULL CHECK (line_no BETWEEN 1 AND 50),
    menu_item_id uuid NOT NULL REFERENCES menu_items(id) ON DELETE RESTRICT,
    external_item_id text NOT NULL,
    name text NOT NULL,
    unit_price_minor bigint NOT NULL CHECK (unit_price_minor > 0),
    quantity smallint NOT NULL CHECK (quantity BETWEEN 1 AND 100),
    line_total_minor bigint NOT NULL CHECK (line_total_minor > 0),
    PRIMARY KEY (order_id, line_no),
    UNIQUE (order_id, menu_item_id),
    CHECK (line_total_minor = unit_price_minor * quantity)
);

CREATE INDEX menu_items_active_establishment_idx
    ON menu_items (establishment_id, id)
    WHERE is_active;

CREATE INDEX orders_user_history_idx
    ON orders (user_id, created_at DESC, id);

CREATE INDEX orders_integration_cursor_idx
    ON orders (establishment_id, updated_at, id);
