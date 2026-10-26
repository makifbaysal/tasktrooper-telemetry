package infra

// Schemas and seed rows of the demo databases. Every statement is idempotent; bulk rows are only
// inserted when the table is empty.

var pgSchemas = map[string][]string{
	"orders-db": {
		`CREATE TABLE IF NOT EXISTS orders (
		   id bigserial PRIMARY KEY, customer_id bigint NOT NULL, merchant_id text NOT NULL, total numeric(12,2) NOT NULL,
		   currency text NOT NULL DEFAULT 'TRY', status text NOT NULL DEFAULT 'created',
		   created_at timestamptz NOT NULL DEFAULT now(), updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders (customer_id, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS order_items (order_id bigint NOT NULL, sku text NOT NULL, qty int NOT NULL, price numeric(12,2) NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_order_items_order ON order_items (order_id)`,
		`CREATE TABLE IF NOT EXISTS prices (sku text PRIMARY KEY, price numeric(12,2) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cart_items (cart_id bigint NOT NULL, sku text NOT NULL, qty int NOT NULL, PRIMARY KEY (cart_id, sku))`,
		`INSERT INTO prices SELECT 'SKU-' || g, round((10 + random() * 990)::numeric, 2) FROM generate_series(1000, 9999) g
		   WHERE NOT EXISTS (SELECT 1 FROM prices)`,
		`INSERT INTO orders (customer_id, merchant_id, total, status, created_at)
		   SELECT 1 + (random() * 49999)::int, 'm-' || (1000 + (random() * 400)::int), round((20 + random() * 900)::numeric, 2),
		          (ARRAY['created','paid','shipped','delivered'])[1 + (random() * 3)::int], now() - random() * interval '90 days'
		   FROM generate_series(1, 200000) WHERE NOT EXISTS (SELECT 1 FROM orders)`,
		`INSERT INTO cart_items SELECT c, 'SKU-' || (1000 + (random() * 8999)::int), 1 + (random() * 3)::int
		   FROM generate_series(1, 50000) c, generate_series(1, 3) WHERE NOT EXISTS (SELECT 1 FROM cart_items) ON CONFLICT DO NOTHING`,
		`ANALYZE`,
	},
	"payments-db": {
		`CREATE TABLE IF NOT EXISTS charges (
		   id bigserial PRIMARY KEY, order_id bigint NOT NULL, amount numeric(12,2) NOT NULL, currency text NOT NULL,
		   provider text NOT NULL, status text NOT NULL, provider_ref text, created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS idx_charges_provider_ref ON charges (provider_ref)`,
		`CREATE TABLE IF NOT EXISTS refunds (id bigserial PRIMARY KEY, charge_id bigint NOT NULL, amount numeric(12,2) NOT NULL,
		   reason text, created_at timestamptz NOT NULL DEFAULT now())`,
		`INSERT INTO charges (order_id, amount, currency, provider, status, provider_ref)
		   SELECT g, round((20 + random() * 900)::numeric, 2), 'TRY', (ARRAY['iyzico','stripe'])[1 + (g % 2)], 'captured', 'pr_' || md5(g::text)
		   FROM generate_series(1, 100000) g WHERE NOT EXISTS (SELECT 1 FROM charges)`,
		`ANALYZE`,
	},
	"reco-db": {
		`CREATE TABLE IF NOT EXISTS reco_items (user_id int NOT NULL, item_id int NOT NULL, score real NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS items (id int PRIMARY KEY, name text NOT NULL, price numeric(12,2) NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS user_segments (user_id int PRIMARY KEY, segment text NOT NULL)`,
		`INSERT INTO items SELECT g, 'item ' || g, round((5 + random() * 500)::numeric, 2) FROM generate_series(1, 20000) g
		   WHERE NOT EXISTS (SELECT 1 FROM items)`,
		`INSERT INTO user_segments SELECT g, (ARRAY['new','returning','vip','dormant'])[1 + (g % 4)] FROM generate_series(1, 50000) g
		   WHERE NOT EXISTS (SELECT 1 FROM user_segments)`,
		// ~900k rows: the top query seq-scans them once idx_reco_items_user_score is gone (~0.3-0.5 s, like the mock).
		`INSERT INTO reco_items SELECT u, 1 + (random() * 19999)::int, random() FROM generate_series(1, 30000) u, generate_series(1, 30)
		   WHERE NOT EXISTS (SELECT 1 FROM reco_items)`,
		`DELETE FROM reco_items WHERE user_id > 30000`, // shrink data loaded by older versions
		`CREATE INDEX IF NOT EXISTS idx_reco_items_user_score ON reco_items (user_id, score DESC)`,
		`ANALYZE`,
	},
	"catalog-db": {
		`CREATE TABLE IF NOT EXISTS items (id bigserial PRIMARY KEY, sku text UNIQUE NOT NULL, name text NOT NULL,
		   category text NOT NULL, price numeric(12,2) NOT NULL, stock int NOT NULL, updated_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE INDEX IF NOT EXISTS idx_items_category ON items (category, id)`,
		`INSERT INTO items (sku, name, category, price, stock)
		   SELECT 'SKU-' || g, (ARRAY['red','blue','black','white','green'])[1 + g % 5] || ' ' ||
		          (ARRAY['sneaker','jacket','kettle','lamp','backpack','watch','mug','chair'])[1 + g % 8] || ' ' || g,
		          (ARRAY['shoes','apparel','home','accessories'])[1 + g % 4], round((5 + random() * 900)::numeric, 2), (random() * 500)::int
		   FROM generate_series(1000, 40999) g WHERE NOT EXISTS (SELECT 1 FROM items)`,
		`ANALYZE`,
	},
	"auth-db": {
		`CREATE TABLE IF NOT EXISTS users (id bigserial PRIMARY KEY, email text UNIQUE NOT NULL, password_hash text NOT NULL,
		   created_at timestamptz NOT NULL DEFAULT now())`,
		`CREATE TABLE IF NOT EXISTS refresh_tokens (token_hash text PRIMARY KEY, user_id bigint NOT NULL, expires_at timestamptz NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS login_events (id bigserial PRIMARY KEY, user_id bigint, ok boolean NOT NULL, at timestamptz NOT NULL DEFAULT now())`,
		`INSERT INTO users (email, password_hash) SELECT 'user' || g || '@example.com', md5(g::text) FROM generate_series(1, 50000) g
		   WHERE NOT EXISTS (SELECT 1 FROM users)`,
		`INSERT INTO refresh_tokens SELECT md5('rt' || g), g, now() + interval '14 days' FROM generate_series(1, 50000) g
		   WHERE NOT EXISTS (SELECT 1 FROM refresh_tokens)`,
		`ANALYZE`,
	},
}

var mysqlSchema = []string{
	`CREATE TABLE IF NOT EXISTS email_log (
	   id bigint AUTO_INCREMENT PRIMARY KEY, message_id varchar(64) NOT NULL, template varchar(64) NOT NULL,
	   recipient_hash char(32) NOT NULL, status varchar(16) NOT NULL, sent_at datetime NOT NULL, KEY idx_message (message_id))`,
	`CREATE TABLE IF NOT EXISTS invoices (
	   id bigint AUTO_INCREMENT PRIMARY KEY, customer_id bigint NOT NULL, amount decimal(12,2) NOT NULL,
	   status varchar(16) NOT NULL, created_at datetime NOT NULL, KEY idx_status (status))`,
	`CREATE TABLE IF NOT EXISTS unsubscribes (recipient_hash char(32) PRIMARY KEY, at datetime NOT NULL)`,
}
