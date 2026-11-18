package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/calnode/calnode/internal/payments"
)

// Stripe Connect (direct charges): the instance is the platform; each host that should
// receive money for their own paid bookings gets one Stripe connected account
// (express). With Connect enabled, a paid booking's charge carries the host's account
// id (Stripe-Account) and settles there with zero platform fee; the same id makes the
// refund and the session refetch land on the right account. Storage: migration 00073.
//
// Failure stance: a missing/broken mapping NEVER blocks a booking — the charge falls
// back to the platform account (the pre-Connect behavior) and the admin sees the gap
// on this settings page.

// stripeProvider returns the active provider when it is Stripe's (the only provider
// with Connect support), else nil. Stripe-specific admin plumbing reaches the client
// through this type assertion, mirroring how PublishableKey/WebhookConfigured are
// Stripe extensions outside the payments.Provider seam.
func (h *Handler) stripeProvider() *payments.StripeProvider {
	sp, _ := h.getPaymentProvider().(*payments.StripeProvider)
	return sp
}

// connectEnabled reports whether direct charges to host accounts are switched on.
// Read per use (one indexed row) so the setting hot-reloads without a restart.
func (h *Handler) connectEnabled(ctx context.Context) bool {
	var n int
	if err := h.db.QueryRowContext(ctx,
		`SELECT stripe_connect_enabled FROM server_settings WHERE id = 1`).Scan(&n); err != nil {
		return false
	}
	return n != 0
}

// hostPayoutAccount resolves the connected account money for hostID settles on.
// "" means platform: Connect off, host not onboarded, or lookup failed.
func (h *Handler) hostPayoutAccount(ctx context.Context, hostID string) string {
	if hostID == "" || !h.connectEnabled(ctx) {
		return ""
	}
	var account string
	if err := h.db.QueryRowContext(ctx,
		`SELECT account_id FROM stripe_connect_accounts WHERE user_id = ?`, hostID).Scan(&account); err != nil {
		return ""
	}
	return account
}

// stripeConnectRow is one host↔connected-account mapping with live status from Stripe.
type stripeConnectRow struct {
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
	UserEmail string `json:"user_email"`
	AccountID string `json:"account_id"`
	// Live status, fetched best-effort at request time (empty when Stripe is
	// unreachable/unconfigured — see StatusError).
	ChargesEnabled   bool     `json:"charges_enabled"`
	PayoutsEnabled   bool     `json:"payouts_enabled"`
	DetailsSubmitted bool     `json:"details_submitted"`
	PastDue          []string `json:"past_due,omitempty"`
	StatusError      string   `json:"status_error,omitempty"`
}

// GetStripeConnectAccounts handles GET /v1/settings/stripe/connect/accounts (admin).
// Returns every mapping plus its live Stripe status, and the enable flag.
func (h *Handler) GetStripeConnectAccounts(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	rows, err := h.db.QueryContext(r.Context(), `
		SELECT c.user_id, u.name, u.email, c.account_id
		FROM stripe_connect_accounts c JOIN users u ON u.id = c.user_id
		ORDER BY u.name`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "stripe connect: list", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	sp := h.stripeProvider()
	out := []stripeConnectRow{}
	for rows.Next() {
		var row stripeConnectRow
		if err := rows.Scan(&row.UserID, &row.UserName, &row.UserEmail, &row.AccountID); err != nil {
			h.logger.ErrorContext(r.Context(), "stripe connect: scan", "error", err)
			continue
		}
		if sp == nil {
			row.StatusError = "stripe not configured"
		} else {
			charges, payouts, details, pastDue, err := sp.ConnectAccountStatus(r.Context(), row.AccountID)
			if err != nil {
				// Stale/unreachable account must not blank the list — show the row
				// with the error so admins can still unlink it.
				row.StatusError = err.Error()
			} else {
				row.ChargesEnabled, row.PayoutsEnabled, row.DetailsSubmitted, row.PastDue =
					charges, payouts, details, pastDue
			}
		}
		out = append(out, row)
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"accounts": out,
		"enabled":  h.connectEnabled(r.Context()),
	})
}

// CreateStripeConnectAccount handles POST /v1/settings/stripe/connect/accounts (admin).
// Body: {"user_id": "...", "business_name": "..."} — creates the Stripe express
// account (idempotent per user, so a double-click cannot mint two) and stores the
// mapping. Does not start onboarding; use the link endpoint for that.
func (h *Handler) CreateStripeConnectAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		UserID       string `json:"user_id"`
		BusinessName string `json:"business_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.UserID) == "" {
		h.writeError(w, http.StatusBadRequest, "user_id required")
		return
	}

	// The host must exist; their name doubles as the default business name.
	var userName string
	if err := h.db.QueryRowContext(r.Context(), `SELECT name FROM users WHERE id = ?`, req.UserID).Scan(&userName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			h.writeError(w, http.StatusNotFound, "user not found")
		} else {
			h.logger.ErrorContext(r.Context(), "stripe connect: user lookup", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	var existing string
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT account_id FROM stripe_connect_accounts WHERE user_id = ?`, req.UserID).Scan(&existing); err == nil {
		h.writeError(w, http.StatusConflict, "user already has a connected account")
		return
	}
	sp := h.stripeProvider()
	if sp == nil {
		h.writeError(w, http.StatusServiceUnavailable, "stripe not configured")
		return
	}

	business := strings.TrimSpace(req.BusinessName)
	if business == "" {
		business = userName
	}
	acctID, err := sp.ConnectCreateAccount(r.Context(), "connect:"+req.UserID, business, h.publicURL())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "stripe connect: create account", "error", err, "user_id", req.UserID)
		h.writeError(w, http.StatusBadGateway, "stripe rejected the request: "+err.Error())
		return
	}
	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO stripe_connect_accounts (user_id, account_id, updated_at)
		VALUES (?, ?, datetime('now'))
		ON CONFLICT(user_id) DO UPDATE SET account_id = excluded.account_id, updated_at = excluded.updated_at`,
		req.UserID, acctID); err != nil {
		h.logger.ErrorContext(r.Context(), "stripe connect: store mapping", "error", err, "user_id", req.UserID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	h.logger.InfoContext(r.Context(), "stripe connect: account created", "user_id", req.UserID, "account", acctID)
	h.writeJSON(w, http.StatusCreated, map[string]any{"user_id": req.UserID, "account_id": acctID})
}

// CreateStripeConnectLink handles POST /v1/settings/stripe/connect/links (admin).
// Body: {"user_id": "..."} → {"url": "..."}: the Stripe-hosted onboarding page the
// host must complete before money can settle to them. Both return URLs land back on
// the payments settings page.
func (h *Handler) CreateStripeConnectLink(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.UserID == "" {
		h.writeError(w, http.StatusBadRequest, "user_id required")
		return
	}
	var acctID string
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT account_id FROM stripe_connect_accounts WHERE user_id = ?`, req.UserID).Scan(&acctID); err != nil {
		h.writeError(w, http.StatusNotFound, "no connected account for this user")
		return
	}
	sp := h.stripeProvider()
	if sp == nil {
		h.writeError(w, http.StatusServiceUnavailable, "stripe not configured")
		return
	}
	base := h.publicURL() + "/admin/settings/payments"
	link, err := sp.ConnectAccountLink(r.Context(), acctID,
		base+"?connect=refresh&user="+url.QueryEscape(req.UserID),
		base+"?connect=return&user="+url.QueryEscape(req.UserID))
	if err != nil {
		h.logger.ErrorContext(r.Context(), "stripe connect: account link", "error", err, "user_id", req.UserID)
		h.writeError(w, http.StatusBadGateway, "stripe rejected the request: "+err.Error())
		return
	}
	h.writeJSON(w, http.StatusOK, map[string]any{"url": link})
}

// DeleteStripeConnectAccount handles DELETE /v1/settings/stripe/connect/accounts?user_id=…
// (admin). Removes the local mapping only — the Stripe account itself (and its
// balance/history) is left untouched by design; deprovisioning is a Stripe-side task.
// Future bookings then settle on the platform account again.
func (h *Handler) DeleteStripeConnectAccount(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	userID := r.URL.Query().Get("user_id")
	if userID == "" {
		h.writeError(w, http.StatusBadRequest, "user_id required")
		return
	}
	res, err := h.db.ExecContext(r.Context(),
		`DELETE FROM stripe_connect_accounts WHERE user_id = ?`, userID)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "stripe connect: unlink", "error", err, "user_id", userID)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "no connected account for this user")
		return
	}
	h.logger.InfoContext(r.Context(), "stripe connect: unlinked", "user_id", userID)
	w.WriteHeader(http.StatusNoContent)
}
