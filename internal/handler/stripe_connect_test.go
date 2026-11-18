package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/payments"
)

// connectTestStripe points the Stripe client at a fake covering the v2 Connect
// endpoints; every request's headers are captured for assertions.
type connectWire struct {
	header http.Header
	path   string
}

func connectTestStripe(t *testing.T, h *Handler, w *connectWire) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.path = r.URL.Path
		w.header = r.Header.Clone()
		rw.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v2/accounts" && r.Method == http.MethodPost:
			_, _ = rw.Write([]byte(`{"id":"acct_9","type":"express"}`))
		case r.URL.Path == "/v2/account_links":
			_, _ = rw.Write([]byte(`{"url":"https://connect.stripe.com/setup/c/x"}`))
		case strings.HasPrefix(r.URL.Path, "/v2/accounts/"):
			_, _ = rw.Write([]byte(`{"id":"acct_9","charges_enabled":true,"payouts_enabled":false,"details_submitted":true,"requirements":{"past_due":[],"currently_due":[],"eventually_due":[]}}`))
		case strings.HasPrefix(r.URL.Path, "/v1/checkout/sessions"):
			_, _ = rw.Write([]byte(`{"id":"cs_connect","url":"https://checkout.test/cs_connect","payment_status":"unpaid"}`))
		default:
			_, _ = rw.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	sc, err := payments.NewStripe("sk_test_x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	sc.SetAPIBase(srv.URL)
	h.SetPaymentProvider(sc)
}

func enableConnect(t *testing.T, db *sql.DB, hostID string, withMapping bool) {
	t.Helper()
	if _, err := db.Exec(`UPDATE server_settings SET stripe_connect_enabled = 1 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if withMapping {
		if _, err := db.Exec(`INSERT INTO stripe_connect_accounts (user_id, account_id) VALUES (?, 'acct_H')`, hostID); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHostPayoutAccount(t *testing.T) {
	h, db, owner := newRefundTestSetup(t)
	ctx := context.Background()

	// Off by default: always platform.
	if got := h.hostPayoutAccount(ctx, owner); got != "" {
		t.Fatalf("disabled = %q; want platform", got)
	}
	enableConnect(t, db, owner, false)
	if got := h.hostPayoutAccount(ctx, owner); got != "" {
		t.Fatalf("on, unmapped = %q; want platform fallback", got)
	}
	enableConnect(t, db, owner, true)
	if got := h.hostPayoutAccount(ctx, owner); got != "acct_H" {
		t.Fatalf("on, mapped = %q; want acct_H", got)
	}
	if got := h.hostPayoutAccount(ctx, ""); got != "" {
		t.Fatalf("empty host = %q; want platform", got)
	}
}

// The whole point of the surgery: with Connect on and the host onboarded, the
// Checkout Session is created ON the host's account and the booking remembers it.
func TestStartBookingCheckout_directCharge(t *testing.T) {
	h, db, owner := newRefundTestSetup(t)
	enableConnect(t, db, owner, true)
	if _, err := db.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at)
		VALUES ('b-cc','et-pay',?,'2099-01-02T10:00:00Z','2099-01-02T10:30:00Z')`, owner); err != nil {
		t.Fatal(err)
	}
	var wire connectWire
	connectTestStripe(t, h, &wire)

	url, err := h.startBookingCheckout(context.Background(), h.getPaymentProvider(), "b-cc", 5000, "aud", "Intro call", "et-pay", "booker@test.com")
	if err != nil {
		t.Fatalf("startBookingCheckout: %v", err)
	}
	if url != "https://checkout.test/cs_connect" {
		t.Fatalf("url = %q", url)
	}
	if got := wire.header.Get("Stripe-Account"); got != "acct_H" {
		t.Fatalf("checkout Stripe-Account = %q; want acct_H", got)
	}
	var acct, session, status string
	if err := db.QueryRow(`SELECT payment_account, payment_session_id, payment_status FROM bookings WHERE id='b-cc'`).
		Scan(&acct, &session, &status); err != nil {
		t.Fatal(err)
	}
	if acct != "acct_H" || session != "cs_connect" || status != "pending" {
		t.Fatalf("booking row = (%q,%q,%q); want (acct_H, cs_connect, pending)", acct, session, status)
	}
}

func TestStartBookingCheckout_platformWhenConnectOff(t *testing.T) {
	h, db, owner := newRefundTestSetup(t)
	if _, err := db.Exec(`INSERT INTO bookings (id,event_type_id,host_id,start_at,end_at)
		VALUES ('b-cc2','et-pay',?,'2099-01-02T10:00:00Z','2099-01-02T10:30:00Z')`, owner); err != nil {
		t.Fatal(err)
	}
	var wire connectWire
	connectTestStripe(t, h, &wire)

	if _, err := h.startBookingCheckout(context.Background(), h.getPaymentProvider(), "b-cc2", 5000, "aud", "Intro call", "et-pay", "booker@test.com"); err != nil {
		t.Fatalf("startBookingCheckout: %v", err)
	}
	if vs := wire.header.Values("Stripe-Account"); len(vs) != 0 {
		t.Fatalf("connect off must omit Stripe-Account; got %v", vs)
	}
	var acct string
	if err := db.QueryRow(`SELECT payment_account FROM bookings WHERE id='b-cc2'`).Scan(&acct); err != nil {
		t.Fatal(err)
	}
	if acct != "" {
		t.Fatalf("payment_account = %q; want platform ('')", acct)
	}
}

// A refund must be issued on the account that received the money — refunding a
// direct charge from the platform account alone fails at Stripe.
func TestRefundBookingPayment_carriesStoredAccount(t *testing.T) {
	h, db, _ := newRefundTestSetup(t)
	if _, err := db.Exec(`UPDATE bookings SET payment_account = 'acct_H' WHERE id='b-pay'`); err != nil {
		t.Fatal(err)
	}
	var wire connectWire
	connectTestStripe(t, h, &wire)

	h.refundBookingPayment(context.Background(), "b-pay")
	if wire.path != "/v1/refunds" {
		t.Fatalf("path = %q; want /v1/refunds", wire.path)
	}
	if got := wire.header.Get("Stripe-Account"); got != "acct_H" {
		t.Fatalf("refund Stripe-Account = %q; want acct_H", got)
	}
	var status string
	db.QueryRow(`SELECT payment_status FROM bookings WHERE id='b-pay'`).Scan(&status)
	if status != "refunded" {
		t.Fatalf("payment_status = %q; want refunded", status)
	}
}

// ---------------------------------------------------------------------------
// Admin CRUD over the real RequireAuth+requireAdmin path (session cookie).
// ---------------------------------------------------------------------------

// seedConnectSession inserts a live session row and returns the cookie value.
// (The external-package tests have their own seedSession; same-package tests need this.)
func seedConnectSession(t *testing.T, db *sql.DB, userID string) string {
	t.Helper()
	sessID := "connect-sess-" + userID
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO sessions (id, user_id, expires_at) VALUES (?, ?, ?)`, sessID, userID, expires); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return sessID
}

func adminRequest(method, target, body, sessID string) *http.Request {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.AddCookie(&http.Cookie{Name: "calnode_session", Value: sessID})
	return r
}

func TestStripeConnectAdminFlow(t *testing.T) {
	h, db, owner := newRefundTestSetup(t)
	sess := seedConnectSession(t, db, owner)
	var wire connectWire
	connectTestStripe(t, h, &wire)

	// Create → 201 + mapping stored.
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateStripeConnectAccount)(rec, adminRequest(http.MethodPost, "/v1/settings/stripe/connect/accounts",
		`{"user_id":"`+owner+`","business_name":"Piee Group"}`, sess))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	if wire.path != "/v2/accounts" {
		t.Fatalf("create hit %q; want /v2/accounts", wire.path)
	}
	var mapped string
	if err := db.QueryRow(`SELECT account_id FROM stripe_connect_accounts WHERE user_id=?`, owner).Scan(&mapped); err != nil || mapped != "acct_9" {
		t.Fatalf("mapping = %q err %v; want acct_9", mapped, err)
	}

	// Duplicate → 409 (the local row, not Stripe, is the gate).
	rec = httptest.NewRecorder()
	h.RequireAuth(h.CreateStripeConnectAccount)(rec, adminRequest(http.MethodPost, "/v1/settings/stripe/connect/accounts", `{"user_id":"`+owner+`"}`, sess))
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d; want 409", rec.Code)
	}

	// Unknown user → 404, no Stripe call at all.
	wire.path = ""
	rec = httptest.NewRecorder()
	h.RequireAuth(h.CreateStripeConnectAccount)(rec, adminRequest(http.MethodPost, "/v1/settings/stripe/connect/accounts", `{"user_id":"ghost"}`, sess))
	if rec.Code != http.StatusNotFound || wire.path != "" {
		t.Fatalf("ghost = %d path %q; want 404 and no stripe call", rec.Code, wire.path)
	}

	// Onboarding link → url.
	rec = httptest.NewRecorder()
	h.RequireAuth(h.CreateStripeConnectLink)(rec, adminRequest(http.MethodPost, "/v1/settings/stripe/connect/links", `{"user_id":"`+owner+`"}`, sess))
	var linkBody struct {
		URL string `json:"url"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &linkBody) != nil || linkBody.URL == "" {
		t.Fatalf("link = %d %s", rec.Code, rec.Body.String())
	}

	// List carries the live status fetched from the fake.
	enableConnect(t, db, owner, false) // flip the flag independently (row already exists)
	rec = httptest.NewRecorder()
	h.RequireAuth(h.GetStripeConnectAccounts)(rec, adminRequest(http.MethodGet, "/v1/settings/stripe/connect/accounts", "", sess))
	var list struct {
		Accounts []struct {
			AccountID      string `json:"account_id"`
			ChargesEnabled bool   `json:"charges_enabled"`
			StatusError    string `json:"status_error"`
		} `json:"accounts"`
		Enabled bool `json:"enabled"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &list) != nil {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}
	if !list.Enabled || len(list.Accounts) != 1 || list.Accounts[0].AccountID != "acct_9" ||
		!list.Accounts[0].ChargesEnabled || list.Accounts[0].StatusError != "" {
		t.Fatalf("list decoded wrong: %+v", list)
	}

	// Unlink → 204, then 404. Bookings fall back to the platform account.
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteStripeConnectAccount)(rec, adminRequest(http.MethodDelete, "/v1/settings/stripe/connect/accounts?user_id="+owner, "", sess))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d; want 204", rec.Code)
	}
	if h.hostPayoutAccount(context.Background(), owner) != "" {
		t.Fatal("after unlink, money must go to platform")
	}
	rec = httptest.NewRecorder()
	h.RequireAuth(h.DeleteStripeConnectAccount)(rec, adminRequest(http.MethodDelete, "/v1/settings/stripe/connect/accounts?user_id="+owner, "", sess))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("re-delete = %d; want 404", rec.Code)
	}
}

func TestPatchStripeSettings_connectToggle(t *testing.T) {
	h, db, owner := newRefundTestSetup(t)
	sess := seedConnectSession(t, db, owner)

	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchStripeSettings)(rec, adminRequest(http.MethodPatch, "/v1/settings/stripe", `{"connect_enabled":true}`, sess))
	if rec.Code != http.StatusOK {
		t.Fatalf("toggle = %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["connect_enabled"] != true {
		t.Fatalf("connect_enabled = %v; want true", resp["connect_enabled"])
	}
	if resp["secret_key_set"] != false {
		t.Fatalf("toggle alone must not touch credentials; secret_key_set = %v", resp["secret_key_set"])
	}
}
