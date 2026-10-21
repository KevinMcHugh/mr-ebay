package ebay_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
)

func client(fake *ebaytest.Server) *ebay.Client {
	return ebay.New(ebay.Config{ClientID: ebaytest.ClientID, ClientSecret: ebaytest.ClientSecret, BaseURL: fake.URL})
}

func TestSearchMapsItems(t *testing.T) {
	fake := ebaytest.New(t)
	auction := ebaytest.Item("v1|2|0", "Beta AR, auction", "")
	delete(auction, "price")
	auction["currentBidPrice"] = map[string]any{"value": "61.5", "currency": "USD"}
	auction["buyingOptions"] = []string{"AUCTION"}
	auction["shippingOptions"] = []map[string]any{{"shippingCostType": "CALCULATED"}}
	fake.Items = []map[string]any{ebaytest.Item("v1|1|0", "Arcteryx Beta jacket M", "149.99"), auction}

	res, err := client(fake).Search(context.Background(), ebay.Query{Keywords: "arcteryx beta"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Total != 2 || len(res.Items) != 2 {
		t.Fatalf("result %+v", res)
	}
	it := res.Items[0]
	if it.ItemID != "v1|1|0" || it.Title != "Arcteryx Beta jacket M" || it.PriceCents != 14999 || it.Currency != "USD" ||
		it.ShippingCents == nil || *it.ShippingCents != 0 || it.TotalCents() != 14999 ||
		it.Seller != "seller_v1|1|0" || it.SellerFeedbackPct == nil || *it.SellerFeedbackPct != 99.8 ||
		it.URL != "https://www.ebay.com/itm/v1|1|0" || it.ImageURL == "" || it.Condition != "Pre-owned" ||
		!slices.Equal(it.BuyingOptions, []string{"FIXED_PRICE"}) || len(it.Raw) == 0 {
		t.Fatalf("item %+v", it)
	}
	if it.EndsAt == nil || !it.EndsAt.Equal(time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)) {
		t.Fatalf("ends %v", it.EndsAt)
	}
	// An auction with no Buy It Now is priced at its current bid, and
	// calculated shipping is unknown, not free.
	if a := res.Items[1]; a.PriceCents != 6150 || a.ShippingCents != nil || a.TotalCents() != 6150 {
		t.Fatalf("auction %+v", a)
	}
}

func TestSearchSendsTheQuery(t *testing.T) {
	fake := ebaytest.New(t)
	_, err := client(fake).Search(context.Background(), ebay.Query{
		Keywords: "rain shell", Conditions: []string{"USED"}, BuyingOptions: []string{"FIXED_PRICE", "BEST_OFFER"},
		MinPriceCents: 1000, MaxPriceCents: 20000, Marketplace: "EBAY_GB", Currency: "GBP",
	})
	if err != nil {
		t.Fatal(err)
	}
	q := fake.Searches()[0]
	if q.Get("q") != "rain shell" || q.Get("category_ids") != "" || q.Get("limit") != "200" || q.Get("offset") != "0" {
		t.Fatalf("query %v", q)
	}
	if want := "buyingOptions:{BEST_OFFER|FIXED_PRICE},conditions:{USED},price:[10.00..200.00],priceCurrency:GBP"; q.Get("filter") != want {
		t.Fatalf("filter %q, want %q", q.Get("filter"), want)
	}
}

func TestFilter(t *testing.T) {
	for _, tc := range []struct {
		q    ebay.Query
		want string
	}{
		// "Any" buying option is sent explicitly: left out, eBay returns
		// fixed-price listings only.
		{ebay.Query{}, "buyingOptions:{AUCTION|BEST_OFFER|FIXED_PRICE}"},
		{ebay.Query{MinPriceCents: 1050}, "buyingOptions:{AUCTION|BEST_OFFER|FIXED_PRICE},price:[10.50],priceCurrency:USD"},
		{ebay.Query{MaxPriceCents: 5000}, "buyingOptions:{AUCTION|BEST_OFFER|FIXED_PRICE},price:[..50.00],priceCurrency:USD"},
		{ebay.Query{BuyingOptions: []string{"AUCTION", "AUCTION"}, Conditions: []string{"USED", "NEW"}},
			"buyingOptions:{AUCTION},conditions:{NEW|USED}"},
	} {
		if got := ebay.Filter(tc.q); got != tc.want {
			t.Errorf("Filter(%+v) = %q, want %q", tc.q, got, tc.want)
		}
	}
}

func TestSearchPages(t *testing.T) {
	fake := ebaytest.New(t)
	for i := range 450 {
		fake.Items = append(fake.Items, ebaytest.Item(fmt.Sprintf("v1|%d|0", i), "item", "1.00"))
	}
	c := client(fake)

	res, err := c.Search(context.Background(), ebay.Query{Keywords: "x", MaxItems: 450})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 450 || res.Total != 450 || res.Items[449].ItemID != "v1|449|0" {
		t.Fatalf("got %d items, total %d", len(res.Items), res.Total)
	}
	var offsets, limits []string
	for _, q := range fake.Searches() {
		offsets, limits = append(offsets, q.Get("offset")), append(limits, q.Get("limit"))
	}
	if !slices.Equal(offsets, []string{"0", "200", "400"}) || !slices.Equal(limits, []string{"200", "200", "50"}) {
		t.Fatalf("offsets %v limits %v", offsets, limits)
	}

	// The default stops at one page, however many eBay has.
	res, err = c.Search(context.Background(), ebay.Query{Keywords: "x"})
	if err != nil || len(res.Items) != ebay.DefaultMaxItems || res.Total != 450 {
		t.Fatalf("default: %d items, total %d, %v", len(res.Items), res.Total, err)
	}
	// No results is an empty list, not nil.
	fake.Items = nil
	if res, err = c.Search(context.Background(), ebay.Query{Keywords: "x"}); err != nil || res.Items == nil || len(res.Items) != 0 {
		t.Fatalf("empty: %+v %v", res, err)
	}
}

func TestSearchMergesCategories(t *testing.T) {
	fake := ebaytest.New(t)
	fake.Items = []map[string]any{
		ebaytest.Categories(ebaytest.Item("a", "in both", "5.00"), "1", "2"),
		ebaytest.Categories(ebaytest.Item("b", "only 2", "5.00"), "2"),
		ebaytest.Categories(ebaytest.Item("c", "neither", "5.00"), "3"),
	}
	res, err := client(fake).Search(context.Background(), ebay.Query{Keywords: "x", CategoryIDs: []string{"1", "2"}})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range res.Items {
		ids = append(ids, it.ItemID)
	}
	// One request per category, and an item in both appears once.
	if !slices.Equal(ids, []string{"a", "b"}) || len(fake.Searches()) != 2 {
		t.Fatalf("ids %v after %d requests", ids, len(fake.Searches()))
	}
}

func TestTokenIsCachedAndRefreshed(t *testing.T) {
	fake := ebaytest.New(t)
	c := client(fake)
	ctx := context.Background()
	for range 3 {
		if _, err := c.Search(ctx, ebay.Query{Keywords: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if fake.Tokens() != 1 {
		t.Fatalf("minted %d tokens for 3 searches, want 1", fake.Tokens())
	}

	// A token eBay stops honoring early is replaced once, and the search
	// still succeeds.
	fake.RevokeNext = true
	if _, err := c.Search(ctx, ebay.Query{Keywords: "x"}); err != nil {
		t.Fatal(err)
	}
	if fake.Tokens() != 2 {
		t.Fatalf("minted %d tokens, want 2 after a revoked one", fake.Tokens())
	}

	// A token that expires within the refresh margin is renewed up front.
	fake.ExpiresIn = 60
	fresh := client(fake)
	for range 2 {
		if _, err := fresh.Search(ctx, ebay.Query{Keywords: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if fake.Tokens() != 4 {
		t.Fatalf("minted %d tokens, want 4 with a 60s lifetime", fake.Tokens())
	}
}

func TestErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := ebay.New(ebay.Config{}).Search(ctx, ebay.Query{}); !errors.Is(err, ebay.ErrNotConfigured) {
		t.Fatalf("no credentials: %v", err)
	}
	t.Setenv("EBAY_CLIENT_ID", "")
	if _, err := ebay.FromEnv(); !errors.Is(err, ebay.ErrNotConfigured) {
		t.Fatalf("FromEnv: %v", err)
	}

	fake := ebaytest.New(t)
	bad := ebay.New(ebay.Config{ClientID: "nope", ClientSecret: "nope", BaseURL: fake.URL})
	var apiErr *ebay.APIError
	if _, err := bad.Search(ctx, ebay.Query{Keywords: "x"}); !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("bad credentials: %v", err)
	}

	fake.Fail = http.StatusInternalServerError
	_, err := client(fake).Search(ctx, ebay.Query{Keywords: "x"})
	if !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Messages[0] != "A system error has occurred." {
		t.Fatalf("server error: %v", err)
	}
	fake.Fail = http.StatusTooManyRequests
	if _, err := client(fake).Search(ctx, ebay.Query{Keywords: "x"}); !errors.Is(err, ebay.ErrRateLimited) {
		t.Fatalf("rate limit: %v", err)
	}

	// An item eBay sends with no price is an error, not a free listing.
	fake.Fail = 0
	noPrice := ebaytest.Item("x", "x", "1.00")
	delete(noPrice, "price")
	fake.Items = []map[string]any{noPrice}
	if _, err := client(fake).Search(ctx, ebay.Query{Keywords: "x"}); err == nil {
		t.Fatal("an item with no price should fail")
	}
}

func TestCents(t *testing.T) {
	for in, want := range map[string]int64{
		"12": 1200, "12.5": 1250, "12.50": 1250, "0.99": 99, ".5": 50, "1,234.00": 123400, "1.005": 101, "1.004": 100,
	} {
		if got, err := ebay.Cents(in); err != nil || got != want {
			t.Errorf("Cents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "abc", "-1.00", "1.2x", "1.234abc", "1..2"} {
		if _, err := ebay.Cents(in); err == nil {
			t.Errorf("Cents(%q) should fail", in)
		}
	}
	if ebay.Decimal(5) != "0.05" || ebay.Decimal(123400) != "1234.00" {
		t.Error("Decimal")
	}
}

func TestConcurrentSearchesShareOneToken(t *testing.T) {
	fake := ebaytest.New(t)
	c := client(fake)
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			_, err := c.Search(context.Background(), ebay.Query{Keywords: "x"})
			errs <- err
		}()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if fake.Tokens() != 1 {
		t.Fatalf("minted %d tokens for 8 concurrent searches, want 1", fake.Tokens())
	}
}
