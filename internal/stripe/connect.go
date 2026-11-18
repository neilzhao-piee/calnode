package stripe

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// Connect (direct charges) support. The instance is the platform; each host that should
// receive money gets its own connected account. Checkout/refund calls carry that account
// id (see Client.doAcct), so funds settle to the host and the platform collects nothing.
//
// Onboarding uses the current v2 surface — POST /v2/accounts + POST /v2/account_links.
// The v1 account-links login path is deprecated for new integrations; Express accounts
// complete their own onboarding on Stripe-hosted pages, and the platform may only create
// and link them, never mutate them.

// ConnectAccountParams creates a connected account for one host.
type ConnectAccountParams struct {
	// Type is the account type: "express" (Stripe handles onboarding + payouts UI).
	// Defaults to "express" when empty.
	Type string
	// Country, e.g. "AU". Optional: Stripe defaults to the platform's country.
	Country string
	// Name and URL populate business_profile; the host can correct them during
	// onboarding. ProductCategory is the Stripe business vertical (e.g.
	// "professional_services"); empty omits it.
	ProductCategory string
	// IdempotencyKey guards double-clicks: Stripe replays the original account.
	IdempotencyKey string
}

// ConnectAccountStatus is the subset of a connected account we surface to admins:
// whether onboarding is complete and whether money can actually move.
type ConnectAccountStatus struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	Country          string `json:"country"`
	Email            string `json:"email"`
	DetailsSubmitted bool   `json:"details_submitted"`
	ChargesEnabled   bool   `json:"charges_enabled"`
	PayoutsEnabled   bool   `json:"payouts_enabled"`
	Requirements     struct {
		PastDue       []string `json:"past_due"`
		CurrentlyDue  []string `json:"currently_due"`
		EventuallyDue []string `json:"eventually_due"`
	} `json:"requirements"`
}

// CreateConnectAccount registers a connected account with Stripe (POST /v2/accounts).
// It does NOT start onboarding — call CreateAccountLink for the hosted onboarding URL.
func (c *Client) CreateConnectAccount(ctx context.Context, p ConnectAccountParams, businessName, businessURL string) (*ConnectAccountStatus, error) {
	acctType := p.Type
	if acctType == "" {
		acctType = "express"
	}
	form := url.Values{}
	form.Set("type", acctType)
	if p.Country != "" {
		form.Set("country", p.Country)
	}
	if businessName != "" {
		form.Set("business_profile[name]", businessName)
	}
	if businessURL != "" {
		form.Set("business_profile[url]", businessURL)
	}
	if p.ProductCategory != "" {
		form.Set("business_profile[product_category]", p.ProductCategory)
	}
	var out ConnectAccountStatus
	key := p.IdempotencyKey
	if err := c.doAcct(ctx, http.MethodPost, "/v2/accounts", form, key, "", &out); err != nil {
		return nil, fmt.Errorf("stripe: create connected account: %w", err)
	}
	return &out, nil
}

// CreateAccountLink returns the Stripe-hosted onboarding URL for a connected account
// (POST /v2/account_links). The host completes verification there; refresh_url /
// return_url bring them back to the admin settings page.
func (c *Client) CreateAccountLink(ctx context.Context, accountID, refreshURL, returnURL string) (string, error) {
	if accountID == "" {
		return "", fmt.Errorf("stripe: account id required")
	}
	form := url.Values{}
	form.Set("account", accountID)
	form.Set("refresh_url", refreshURL)
	form.Set("return_url", returnURL)
	var out struct {
		URL string `json:"url"`
	}
	if err := c.doAcct(ctx, http.MethodPost, "/v2/account_links", form, "", "", &out); err != nil {
		return "", fmt.Errorf("stripe: create account link: %w", err)
	}
	return out.URL, nil
}

// GetConnectAccount fetches the current status of a connected account (GET /v2/accounts/{id}).
func (c *Client) GetConnectAccount(ctx context.Context, accountID string) (*ConnectAccountStatus, error) {
	var out ConnectAccountStatus
	if err := c.doAcct(ctx, http.MethodGet, "/v2/accounts/"+url.PathEscape(accountID), nil, "", "", &out); err != nil {
		return nil, fmt.Errorf("stripe: fetch connected account: %w", err)
	}
	return &out, nil
}
