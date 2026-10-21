// Package ebay is the app's one door to eBay: an application token from the
// OAuth client-credentials grant, and the Browse API's item_summary/search.
// See docs/design.md, "eBay integration".
//
// Endpoints never import the HTTP client directly. They take a Searcher, as
// they take a narrow Store, so tests hand them the fake in package ebaytest.
package ebay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Hosts. Sandbox listings are test data, useful only to prove credentials.
const (
	ProductionBase = "https://api.ebay.com"
	SandboxBase    = "https://api.sandbox.ebay.com"
)

// Scope is the one OAuth scope the Browse API's search needs.
const Scope = "https://api.ebay.com/oauth/api_scope"

// Limits.
const (
	// PageSize is the most items one search request returns.
	PageSize = 200
	// DefaultMaxItems is how many items a Search fetches when the query does
	// not say: one full page.
	DefaultMaxItems = 200
	// MaxItems caps a single Search, to keep one run from eating the day's
	// call quota.
	MaxItems = 1000
)

var (
	// ErrNotConfigured is returned by FromEnv, and by a nil-config client,
	// when there are no eBay credentials. The app still runs without them.
	ErrNotConfigured = errors.New("eBay is not configured: set EBAY_CLIENT_ID and EBAY_CLIENT_SECRET")
	// ErrRateLimited is eBay refusing for quota (HTTP 429).
	ErrRateLimited = errors.New("eBay rate limit reached; try again later")
)

// Searcher is what an endpoint needs from eBay.
type Searcher interface {
	Search(ctx context.Context, q Query) (Result, error)
}

// Query is one search, in the app's terms. Prices are cents of Currency,
// and 0 means no limit; an empty set means any.
type Query struct {
	Keywords      string
	CategoryIDs   []string
	Conditions    []string // NEW, USED, UNSPECIFIED
	BuyingOptions []string // FIXED_PRICE, AUCTION, BEST_OFFER
	MinPriceCents int64
	MaxPriceCents int64
	Currency      string // default USD
	Marketplace   string // default EBAY_US
	// MaxItems bounds how many items to fetch across pages: 0 is
	// DefaultMaxItems, and anything above MaxItems is MaxItems.
	MaxItems int
}

// Item is one listing as the app stores it.
type Item struct {
	ItemID            string
	Title             string
	URL               string
	ImageURL          string
	Seller            string
	SellerFeedbackPct *float64
	Condition         string
	BuyingOptions     []string
	// PriceCents is the Buy It Now price, or the current bid for an auction
	// with no Buy It Now.
	PriceCents int64
	// ShippingCents is the first shipping option's cost; nil when eBay did
	// not give one (calculated shipping, say).
	ShippingCents *int64
	Currency      string
	EndsAt        *time.Time
	// Marketplace is the one it was found on, from the query.
	Marketplace string
	// Raw is the item summary exactly as eBay sent it.
	Raw json.RawMessage
}

// TotalCents is price plus shipping, counting unknown shipping as 0.
func (it Item) TotalCents() int64 {
	if it.ShippingCents == nil {
		return it.PriceCents
	}
	return it.PriceCents + *it.ShippingCents
}

// Result is a search's items, de-duplicated, in eBay's order, and how many
// eBay said matched in all (summed across categories).
type Result struct {
	Items []Item
	Total int
}

// APIError is an error response from eBay.
type APIError struct {
	Status   int
	Messages []string
}

func (e *APIError) Error() string {
	msg := strings.Join(e.Messages, "; ")
	if msg == "" {
		msg = http.StatusText(e.Status)
	}
	return fmt.Sprintf("eBay returned %d: %s", e.Status, msg)
}

// -----------------------------------------------------------------------------
// Client
// -----------------------------------------------------------------------------

// Config is what a Client needs.
type Config struct {
	ClientID     string
	ClientSecret string
	// BaseURL is ProductionBase, SandboxBase, or a fake's URL in tests.
	BaseURL string
	// HTTP defaults to a client with a 30 second timeout.
	HTTP *http.Client
}

// Client is the HTTP Searcher. It is safe for concurrent use.
type Client struct {
	cfg Config
	now func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New builds a Client on cfg.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = ProductionBase
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	return &Client{cfg: cfg, now: time.Now}
}

// FromEnv builds a Client from EBAY_CLIENT_ID, EBAY_CLIENT_SECRET and
// EBAY_ENV ("sandbox" for the sandbox; anything else is production), or
// EBAY_API_BASE to point it somewhere else entirely. It returns
// ErrNotConfigured when the credentials are missing.
func FromEnv() (*Client, error) {
	id, secret := os.Getenv("EBAY_CLIENT_ID"), os.Getenv("EBAY_CLIENT_SECRET")
	if id == "" || secret == "" {
		return nil, ErrNotConfigured
	}
	base := ProductionBase
	if os.Getenv("EBAY_ENV") == "sandbox" {
		base = SandboxBase
	}
	if b := os.Getenv("EBAY_API_BASE"); b != "" {
		base = b
	}
	return New(Config{ClientID: id, ClientSecret: secret, BaseURL: base}), nil
}

// Search runs q, paging until it has q.MaxItems or eBay runs out. eBay
// takes one category per request, so a query with several runs once per
// category and merges the results, keeping the first sighting of an item.
func (c *Client) Search(ctx context.Context, q Query) (Result, error) {
	if c == nil || c.cfg.ClientID == "" || c.cfg.ClientSecret == "" {
		return Result{}, ErrNotConfigured
	}
	q = normalize(q)
	cats := q.CategoryIDs
	if len(cats) == 0 {
		cats = []string{""}
	}
	var res Result
	seen := map[string]bool{}
	for _, cat := range cats {
		items, total, err := c.searchCategory(ctx, q, cat, q.MaxItems-len(res.Items))
		if err != nil {
			return Result{}, err
		}
		res.Total += total
		for _, it := range items {
			if !seen[it.ItemID] {
				seen[it.ItemID] = true
				res.Items = append(res.Items, it)
			}
		}
		if len(res.Items) >= q.MaxItems {
			break
		}
	}
	if res.Items == nil {
		res.Items = []Item{}
	}
	return res, nil
}

func (c *Client) searchCategory(ctx context.Context, q Query, category string, want int) ([]Item, int, error) {
	var out []Item
	total := 0
	for offset := 0; len(out) < want; {
		limit := min(PageSize, want-len(out))
		page, err := c.page(ctx, q, category, offset, limit)
		if err != nil {
			return nil, 0, err
		}
		total = page.Total
		for _, raw := range page.ItemSummaries {
			it, err := toItem(raw)
			if err != nil {
				return nil, 0, err
			}
			it.Marketplace = q.Marketplace
			out = append(out, it)
		}
		offset += len(page.ItemSummaries)
		if len(page.ItemSummaries) == 0 || page.Next == "" || offset >= total {
			break
		}
	}
	return out, total, nil
}

// searchPage is the part of eBay's search response the client reads.
type searchPage struct {
	Total         int               `json:"total"`
	Next          string            `json:"next"`
	ItemSummaries []json.RawMessage `json:"itemSummaries"`
}

// page fetches one page. A 401 means the cached token went stale early, so
// it drops the token and tries once more.
func (c *Client) page(ctx context.Context, q Query, category string, offset, limit int) (searchPage, error) {
	v := url.Values{}
	v.Set("q", q.Keywords)
	if category != "" {
		v.Set("category_ids", category)
	}
	v.Set("filter", Filter(q))
	v.Set("limit", strconv.Itoa(limit))
	v.Set("offset", strconv.Itoa(offset))
	endpoint := c.cfg.BaseURL + "/buy/browse/v1/item_summary/search?" + v.Encode()

	for attempt := 0; ; attempt++ {
		token, err := c.accessToken(ctx)
		if err != nil {
			return searchPage{}, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return searchPage{}, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-EBAY-C-MARKETPLACE-ID", q.Marketplace)
		req.Header.Set("Accept", "application/json")
		resp, err := c.cfg.HTTP.Do(req)
		if err != nil {
			return searchPage{}, fmt.Errorf("eBay search: %w", err)
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			resp.Body.Close()
			c.dropToken(token)
			continue
		}
		var p searchPage
		err = decode(resp, &p)
		return p, err
	}
}

// accessToken returns a cached application token, minting a new one when
// there is none or it is within a few minutes of expiring.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "scope": {Scope}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+"/identity/v1/oauth2/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(c.cfg.ClientID, c.cfg.ClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.cfg.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("eBay token: %w", err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := decode(resp, &tok); err != nil {
		return "", fmt.Errorf("eBay token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", errors.New("eBay token: response had no access_token")
	}
	// Refresh five minutes early, so a token never expires mid-run.
	life := time.Duration(tok.ExpiresIn)*time.Second - 5*time.Minute
	c.token, c.expires = tok.AccessToken, c.now().Add(max(life, 0))
	return c.token, nil
}

// dropToken forgets token, unless another request already replaced it.
func (c *Client) dropToken(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
	}
}

// decode reads a JSON response into out, or turns an error response into
// ErrRateLimited or an *APIError carrying eBay's messages.
func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return ErrRateLimited
	}
	if resp.StatusCode/100 != 2 {
		var body struct {
			Errors []struct {
				Message     string `json:"message"`
				LongMessage string `json:"longMessage"`
			} `json:"errors"`
			// The token endpoint answers in OAuth's shape instead.
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		e := &APIError{Status: resp.StatusCode}
		for _, m := range body.Errors {
			msg := m.LongMessage
			if msg == "" {
				msg = m.Message
			}
			e.Messages = append(e.Messages, msg)
		}
		if body.Error != "" {
			e.Messages = append(e.Messages, strings.TrimSpace(body.Error+" "+body.ErrorDescription))
		}
		return e
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("eBay response: %w", err)
	}
	return nil
}

// -----------------------------------------------------------------------------
// Query to filter
// -----------------------------------------------------------------------------

func normalize(q Query) Query {
	if q.Currency == "" {
		q.Currency = "USD"
	}
	if q.Marketplace == "" {
		q.Marketplace = "EBAY_US"
	}
	switch {
	case q.MaxItems <= 0:
		q.MaxItems = DefaultMaxItems
	case q.MaxItems > MaxItems:
		q.MaxItems = MaxItems
	}
	return q
}

// Filter is q's Browse API filter parameter, e.g.
// "buyingOptions:{AUCTION|FIXED_PRICE},conditions:{USED},price:[10.00..200.00],priceCurrency:USD".
//
// An empty BuyingOptions is sent as every option, not left out: left out,
// eBay returns only fixed-price listings, and "any" here has to include
// auctions.
func Filter(q Query) string {
	q = normalize(q)
	opts := q.BuyingOptions
	if len(opts) == 0 {
		opts = []string{"AUCTION", "BEST_OFFER", "FIXED_PRICE"}
	}
	parts := []string{"buyingOptions:{" + strings.Join(sorted(opts), "|") + "}"}
	if len(q.Conditions) > 0 {
		parts = append(parts, "conditions:{"+strings.Join(sorted(q.Conditions), "|")+"}")
	}
	if q.MinPriceCents > 0 || q.MaxPriceCents > 0 {
		lo, hi := "", ""
		if q.MinPriceCents > 0 {
			lo = Decimal(q.MinPriceCents)
		}
		if q.MaxPriceCents > 0 {
			hi = Decimal(q.MaxPriceCents)
		}
		r := lo + ".." + hi
		if hi == "" {
			r = lo // eBay reads [10] as "10 or more"
		}
		parts = append(parts, "price:["+r+"]", "priceCurrency:"+q.Currency)
	}
	return strings.Join(parts, ",")
}

func sorted(vs []string) []string {
	out := slices.Clone(vs)
	slices.Sort(out)
	return slices.Compact(out)
}

// -----------------------------------------------------------------------------
// Response to Item
// -----------------------------------------------------------------------------

type amount struct {
	Value    string `json:"value"`
	Currency string `json:"currency"`
}

// summary is the part of an item summary the app copies into columns. The
// whole thing is kept in Item.Raw.
type summary struct {
	ItemID     string `json:"itemId"`
	Title      string `json:"title"`
	ItemWebURL string `json:"itemWebUrl"`
	Image      struct {
		ImageURL string `json:"imageUrl"`
	} `json:"image"`
	Seller struct {
		Username           string `json:"username"`
		FeedbackPercentage string `json:"feedbackPercentage"`
	} `json:"seller"`
	Condition       string   `json:"condition"`
	BuyingOptions   []string `json:"buyingOptions"`
	Price           *amount  `json:"price"`
	CurrentBidPrice *amount  `json:"currentBidPrice"`
	ShippingOptions []struct {
		ShippingCost *amount `json:"shippingCost"`
	} `json:"shippingOptions"`
	ItemEndDate string `json:"itemEndDate"`
}

func toItem(raw json.RawMessage) (Item, error) {
	var s summary
	if err := json.Unmarshal(raw, &s); err != nil {
		return Item{}, fmt.Errorf("eBay item: %w", err)
	}
	if s.ItemID == "" {
		return Item{}, errors.New("eBay item: no itemId")
	}
	price := s.Price
	if price == nil || price.Value == "" {
		price = s.CurrentBidPrice
	}
	if price == nil || price.Value == "" {
		return Item{}, fmt.Errorf("eBay item %s: no price", s.ItemID)
	}
	cents, err := Cents(price.Value)
	if err != nil {
		return Item{}, fmt.Errorf("eBay item %s: price: %w", s.ItemID, err)
	}
	it := Item{
		ItemID:        s.ItemID,
		Title:         s.Title,
		URL:           s.ItemWebURL,
		ImageURL:      s.Image.ImageURL,
		Seller:        s.Seller.Username,
		Condition:     s.Condition,
		BuyingOptions: s.BuyingOptions,
		PriceCents:    cents,
		Currency:      price.Currency,
		Raw:           raw,
	}
	if it.BuyingOptions == nil {
		it.BuyingOptions = []string{}
	}
	if pct, err := strconv.ParseFloat(s.Seller.FeedbackPercentage, 64); err == nil {
		it.SellerFeedbackPct = &pct
	}
	if len(s.ShippingOptions) > 0 && s.ShippingOptions[0].ShippingCost != nil && s.ShippingOptions[0].ShippingCost.Value != "" {
		ship, err := Cents(s.ShippingOptions[0].ShippingCost.Value)
		if err != nil {
			return Item{}, fmt.Errorf("eBay item %s: shipping: %w", s.ItemID, err)
		}
		it.ShippingCents = &ship
	}
	if s.ItemEndDate != "" {
		if t, err := time.Parse(time.RFC3339, s.ItemEndDate); err == nil {
			it.EndsAt = &t
		}
	}
	return it, nil
}

// Cents parses one of eBay's decimal amounts ("12.5", "1234.00") into
// cents, rounding half up past two places. It never goes through a float.
func Cents(s string) (int64, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "." {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 || strings.Trim(frac, "0123456789") != "" {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	round := len(frac) > 2 && frac[2] >= '5'
	frac = (frac + "00")[:2]
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not an amount", s)
	}
	c := w*100 + f
	if round {
		c++
	}
	return c, nil
}

// Decimal is cents as eBay writes amounts: "12.50".
func Decimal(cents int64) string {
	return fmt.Sprintf("%d.%02d", cents/100, cents%100)
}
