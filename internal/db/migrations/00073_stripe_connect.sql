-- +goose Up
-- Stripe Connect (direct charges): money lands on the ASSIGNED host's connected
-- account, platform fee 0.
--
-- stripe_connect_accounts maps a Calnode host (users.id) to a Stripe connected
-- account created via POST /v2/accounts. bookings.payment_account records the
-- account the charge actually settled on, because refunds and session refetches
-- must be issued on that same account. stripe_connect_enabled gates the feature:
-- while off, every charge settles on the platform account exactly as before.
ALTER TABLE bookings ADD COLUMN payment_account TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN stripe_connect_enabled INTEGER NOT NULL DEFAULT 0;

CREATE TABLE stripe_connect_accounts (
    user_id    TEXT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

-- +goose Down
DROP TABLE stripe_connect_accounts;
ALTER TABLE server_settings DROP COLUMN stripe_connect_enabled;
ALTER TABLE bookings DROP COLUMN payment_account;
