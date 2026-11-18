package payments

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// connectFakeServer records the last request's headers/path for the seam tests.
type connectSeen struct {
	path   string
	header http.Header
}

func connectFakeServer(t *testing.T, resp string, seen *connectSeen) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.path = r.URL.Path
		seen.header = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The seam must carry PayoutAccount all the way to the wire for create, fetch and
// refund — the booking flow only ever speaks payments.* types, so this is where a
// dropped field would silently misroute money to the platform.
func TestStripeProvider_payoutAccountThreadsToWire(t *testing.T) {
	var seen connectSeen
	srv := connectFakeServer(t, `{"id":"cs_1","url":"https://checkout.test/cs_1","payment_status":"paid","payment_intent":"pi_1"}`, &seen)
	p, err := NewStripe("sk_test_x", "", "")
	if err != nil {
		t.Fatal(err)
	}
	p.SetAPIBase(srv.URL)
	ctx := context.Background()

	sess, err := p.CreateCheckout(ctx, CheckoutRequest{
		AmountMinor: 5000, Currency: "AUD", ProductName: "Consult", PayoutAccount: "acct_H",
	})
	if err != nil || sess.ID != "cs_1" {
		t.Fatalf("CreateCheckout: %v %+v", err, sess)
	}
	if got := seen.header.Get("Stripe-Account"); got != "acct_H" {
		t.Fatalf("create Stripe-Account = %q; want acct_H", got)
	}

	if _, err := p.GetSession(ctx, "cs_1", "acct_H"); err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got := seen.header.Get("Stripe-Account"); got != "acct_H" {
		t.Fatalf("get Stripe-Account = %q; want acct_H", got)
	}

	if err := p.Refund(ctx, RefundRequest{PaymentRef: "pi_1", IdempotencyKey: "refund:b1", PayoutAccount: "acct_H"}); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if got := seen.header.Get("Stripe-Account"); got != "acct_H" {
		t.Fatalf("refund Stripe-Account = %q; want acct_H", got)
	}

	// Platform default: no header at all.
	if _, err := p.CreateCheckout(ctx, CheckoutRequest{AmountMinor: 100, Currency: "AUD", ProductName: "P"}); err != nil {
		t.Fatalf("platform CreateCheckout: %v", err)
	}
	if vs := seen.header.Values("Stripe-Account"); len(vs) != 0 {
		t.Fatalf("platform create must omit Stripe-Account; got %v", vs)
	}
}

// Direct-charge events land on the platform endpoint with an "account" tag; the
// normalized Event must surface it.
func TestStripeProvider_parseWebhook_surfacesPayoutAccount(t *testing.T) {
	secret := "whsec_test"
	p, err := NewStripe("sk_test_x", "", secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	payload := []byte(`{"id":"evt_1","type":"checkout.session.completed","account":"acct_H","data":{"object":{"id":"cs_1","payment_status":"paid","payment_intent":"pi_1","metadata":{"booking_id":"bk_1"}}}}`)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(now.Unix(), 10)))
	mac.Write([]byte("."))
	mac.Write(payload)
	sig := fmt.Sprintf("t=%d,v1=%s", now.Unix(), hex.EncodeToString(mac.Sum(nil)))

	ev, err := p.ParseWebhook(payload, http.Header{"Stripe-Signature": []string{sig}}, now)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if ev.Kind != EventCheckoutCompleted {
		t.Fatalf("kind = %v; want EventCheckoutCompleted", ev.Kind)
	}
	if ev.PayoutAccount != "acct_H" {
		t.Fatalf("PayoutAccount = %q; want acct_H", ev.PayoutAccount)
	}
	if ev.Session == nil || ev.Session.PaymentRef != "pi_1" {
		t.Fatalf("session = %+v", ev.Session)
	}
}
