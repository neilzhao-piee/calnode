package stripe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// connectFake starts a fake Stripe endpoint that records the last request for the
// given path prefix and replies with the canned JSON body.
func connectFake(t *testing.T, resp string, got *capturedRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		got.path = r.URL.Path
		got.method = r.Method
		got.header = r.Header.Clone()
		got.form = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return srv
}

type capturedRequest struct {
	path   string
	method string
	header http.Header
	form   map[string][]string
}

func newTestClient(base string) *Client {
	c, err := New("sk_test_x", "", "")
	if err != nil {
		panic(err)
	}
	c.SetAPIBase(base)
	return c
}

// The Stripe-Account header is THE direct-charge mechanism: if it is absent the
// charge silently settles on the platform, so both directions are asserted here.
func TestStripeAccountHeader_onCheckoutRefundGet(t *testing.T) {
	var got capturedRequest
	c := newTestClient(connectFake(t, `{"id":"cs_1","url":"https://checkout.stripe.test/cs_1","payment_status":"paid","payment_intent":"pi_1"}`, &got).URL)

	if _, err := c.CreateCheckoutSession(context.Background(), CheckoutParams{
		AmountCents: 100, Currency: "aud", ProductName: "P", SuccessURL: "https://x/s", CancelURL: "https://x/c",
		StripeAccount: "acct_H",
	}); err != nil {
		t.Fatalf("CreateCheckoutSession: %v", err)
	}
	if h := got.header.Get("Stripe-Account"); h != "acct_H" {
		t.Fatalf("checkout Stripe-Account = %q; want acct_H", h)
	}

	if err := c.Refund(context.Background(), "pi_1", "refund:b1", "acct_H"); err != nil {
		t.Fatalf("Refund: %v", err)
	}
	if h := got.header.Get("Stripe-Account"); h != "acct_H" {
		t.Fatalf("refund Stripe-Account = %q; want acct_H", h)
	}
	if k := got.header.Get("Idempotency-Key"); k != "refund:b1" {
		t.Fatalf("Idempotency-Key = %q; want refund:b1", k)
	}

	if _, err := c.GetCheckoutSession(context.Background(), "cs_1", "acct_H"); err != nil {
		t.Fatalf("GetCheckoutSession: %v", err)
	}
	if h := got.header.Get("Stripe-Account"); h != "acct_H" {
		t.Fatalf("get Stripe-Account = %q; want acct_H", h)
	}

	// Platform mode (no account): header must be ABSENT, not empty-string-ambiguous.
	if _, err := c.CreateCheckoutSession(context.Background(), CheckoutParams{
		AmountCents: 100, Currency: "aud", ProductName: "P", SuccessURL: "https://x/s", CancelURL: "https://x/c",
	}); err != nil {
		t.Fatalf("platform CreateCheckoutSession: %v", err)
	}
	if vs := got.header.Values("Stripe-Account"); len(vs) != 0 {
		t.Fatalf("platform checkout must not send Stripe-Account; got %v", vs)
	}
}

func TestCreateConnectAccount_v2(t *testing.T) {
	var got capturedRequest
	c := newTestClient(connectFake(t, `{"id":"acct_new1","type":"express","country":"AU","charges_enabled":false}`, &got).URL)

	acct, err := c.CreateConnectAccount(context.Background(), ConnectAccountParams{
		IdempotencyKey: "connect:u1",
	}, "Piee Group", "https://booking.test")
	if err != nil {
		t.Fatalf("CreateConnectAccount: %v", err)
	}
	if acct.ID != "acct_new1" {
		t.Fatalf("account id = %q; want acct_new1", acct.ID)
	}
	if got.path != "/v2/accounts" || got.method != http.MethodPost {
		t.Fatalf("hit %s %s; want POST /v2/accounts", got.method, got.path)
	}
	if got.form["type"][0] != "express" {
		t.Errorf("type = %v; want express", got.form["type"])
	}
	if got.form["business_profile[name]"][0] != "Piee Group" {
		t.Errorf("business_profile[name] = %v", got.form["business_profile[name]"])
	}
	if got.form["business_profile[url]"][0] != "https://booking.test" {
		t.Errorf("business_profile[url] = %v", got.form["business_profile[url]"])
	}
	if k := got.header.Get("Idempotency-Key"); k != "connect:u1" {
		t.Errorf("Idempotency-Key = %q; want connect:u1", k)
	}
	// Optional fields must be omitted, not sent empty — Stripe rejects empty enums.
	if _, ok := got.form["country"]; ok {
		t.Errorf("empty country must be omitted")
	}
}

func TestCreateAccountLink_v2(t *testing.T) {
	var got capturedRequest
	c := newTestClient(connectFake(t, `{"url":"https://connect.stripe.com/setup/c/abc"}`, &got).URL)

	link, err := c.CreateAccountLink(context.Background(), "acct_1",
		"https://booking.test/admin/settings/payments?connect=refresh",
		"https://booking.test/admin/settings/payments?connect=return")
	if err != nil {
		t.Fatalf("CreateAccountLink: %v", err)
	}
	if link != "https://connect.stripe.com/setup/c/abc" {
		t.Fatalf("link = %q", link)
	}
	if got.path != "/v2/account_links" {
		t.Fatalf("path = %s; want /v2/account_links", got.path)
	}
	if got.form["account"][0] != "acct_1" {
		t.Errorf("account = %v", got.form["account"])
	}
}

func TestGetConnectAccount_status(t *testing.T) {
	var got capturedRequest
	c := newTestClient(connectFake(t, `{
		"id":"acct_1","type":"express","details_submitted":true,
		"charges_enabled":true,"payouts_enabled":false,
		"requirements":{"past_due":["bank_account"],"currently_due":[],"eventually_due":[]}
	}`, &got).URL)

	st, err := c.GetConnectAccount(context.Background(), "acct_1")
	if err != nil {
		t.Fatalf("GetConnectAccount: %v", err)
	}
	if got.path != "/v2/accounts/acct_1" {
		t.Fatalf("path = %s; want /v2/accounts/acct_1", got.path)
	}
	if !st.DetailsSubmitted || !st.ChargesEnabled || st.PayoutsEnabled {
		t.Errorf("flags decoded wrong: %+v", st)
	}
	if len(st.Requirements.PastDue) != 1 || st.Requirements.PastDue[0] != "bank_account" {
		t.Errorf("requirements decoded wrong: %+v", st.Requirements)
	}
}

// Direct-charge events arrive at the platform endpoint tagged with the connected
// account; the envelope must carry it through for routing/logging.
func TestConstructEvent_carriesAccount(t *testing.T) {
	secret := "whsec_test"
	now := time.Unix(1_700_000_000, 0)
	payload := []byte(`{"id":"evt_1","type":"checkout.session.completed","account":"acct_H","data":{"object":{"id":"cs_1","payment_status":"paid"}}}`)
	ev, err := ConstructEvent(payload, sign(payload, secret, now), secret, now)
	if err != nil {
		t.Fatalf("ConstructEvent: %v", err)
	}
	if ev.Account != "acct_H" {
		t.Fatalf("event account = %q; want acct_H", ev.Account)
	}
	// Sanity: the object still decodes.
	if s, err := ev.Session(); err != nil || s.ID != "cs_1" {
		t.Fatalf("session after account field: %+v, %v", s, err)
	}
	b, _ := json.Marshal(ev)
	if len(b) == 0 {
		t.Fatal("event does not marshal")
	}
}
