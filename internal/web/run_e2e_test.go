package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/runs"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
	"github.com/KevinMcHugh/mr-ebay/internal/web"
)

// TestRunSearch runs a saved search against the fake eBay several times and
// checks what is stored: listings once, matches per search with their
// status kept, price points only on change, and every run in the log.
func TestRunSearch(t *testing.T) {
	e := newEnv(t)
	jacket := ebaytest.Item("v1|1|0", "Arcteryx Beta jacket M", "149.99")
	auction := ebaytest.Item("v1|2|0", "Beta AR, auction", "")
	delete(auction, "price")
	auction["currentBidPrice"] = map[string]any{"value": "61.50", "currency": "USD"}
	auction["buyingOptions"] = []string{"AUCTION"}
	e.ebay.Items = []map[string]any{jacket, auction}

	var sr searches.Search
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "rain shell", "query": "arcteryx beta", "conditions": []string{"USED"}}, http.StatusCreated, &sr)

	// First run: both are new, nothing dropped.
	var run runs.Result
	e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, &run)
	if r := run.Run; r.ResultCount != 2 || r.NewCount != 2 || r.PriceDropCount != 0 || r.Error != "" || r.FinishedAt == nil ||
		len(run.NewMatches) != 2 || len(run.PriceDrops) != 0 || r.Trigger != "manual" {
		t.Fatalf("first run %+v", run)
	}
	m0 := run.NewMatches[0]
	if m0.Status != "new" || m0.Listing.ID == "" || m0.Listing.EbayItemID != "v1|1|0" || m0.Listing.TotalCents != 14999 ||
		m0.Listing.PageURL != e.url+"/listings/"+m0.Listing.ID || m0.Listing.Marketplace != "EBAY_US" {
		t.Fatalf("first match %+v", m0)
	}
	if run.NewMatches[1].Listing.PriceCents != 6150 || run.NewMatches[1].Listing.ShippingCents == nil {
		t.Fatalf("auction %+v", run.NewMatches[1].Listing)
	}
	if f := e.ebay.Searches()[0].Get("filter"); f != "buyingOptions:{AUCTION|BEST_OFFER|FIXED_PRICE},conditions:{USED}" {
		t.Fatalf("filter sent %q", f)
	}
	var got searches.Search
	e.do("GET", "/api/searches/"+sr.ID, e.key, nil, http.StatusOK, &got)
	if got.LastRunAt == nil || got.NewCount != 2 {
		t.Fatalf("search after a run %+v", got)
	}

	// Unchanged: nothing new, nothing dropped.
	e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, &run)
	if run.Run.NewCount != 0 || run.Run.PriceDropCount != 0 || len(run.NewMatches) != 0 || run.Run.ResultCount != 2 {
		t.Fatalf("unchanged run %+v", run)
	}

	// The jacket drops, and a new listing appears.
	jacket["price"] = map[string]any{"value": "120.00", "currency": "USD"}
	e.ebay.Items = append(e.ebay.Items, ebaytest.Item("v1|3|0", "Beta LT", "99.00"))
	e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, &run)
	if run.Run.NewCount != 1 || run.Run.PriceDropCount != 1 || len(run.NewMatches) != 1 || run.NewMatches[0].Listing.EbayItemID != "v1|3|0" {
		t.Fatalf("third run %+v", run)
	}
	if d := run.PriceDrops[0]; d.FromCents != 14999 || d.ToCents != 12000 || d.Match.ID != m0.ID {
		t.Fatalf("drop %+v", d)
	}

	// Statuses: report the jacket, dismiss the auction.
	var all struct{ Matches []matches.Match }
	e.do("GET", "/api/searches/"+sr.ID+"/matches", e.key, nil, http.StatusOK, &all)
	if len(all.Matches) != 3 || all.Matches[0].Listing.EbayItemID != "v1|3|0" {
		t.Fatalf("matches %+v", all)
	}
	ids := map[string]string{}
	for _, m := range all.Matches {
		ids[m.Listing.EbayItemID] = m.ID
	}
	var upd struct{ Updated int64 }
	e.do("POST", "/api/matches/status", e.key, map[string]any{"ids": []string{ids["v1|1|0"]}, "status": "seen"}, http.StatusOK, &upd)
	e.do("POST", "/api/matches/status", e.key, map[string]any{"ids": []string{ids["v1|2|0"], "nope"}, "status": "dismissed"}, http.StatusOK, &upd)
	if upd.Updated != 1 {
		t.Fatalf("an unknown id should be skipped: updated %d", upd.Updated)
	}
	e.do("POST", "/api/matches/status", e.key, map[string]any{"ids": []string{ids["v1|2|0"]}, "status": "archived"}, http.StatusUnprocessableEntity, nil)
	e.do("POST", "/api/matches/status", e.key, map[string]any{"ids": []string{}, "status": "seen"}, http.StatusUnprocessableEntity, nil)

	// A later run keeps those statuses.
	e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, &run)
	var fresh struct{ Matches []matches.Match }
	e.do("GET", "/api/searches/"+sr.ID+"/matches?status=new", e.key, nil, http.StatusOK, &fresh)
	if len(fresh.Matches) != 1 || fresh.Matches[0].Listing.EbayItemID != "v1|3|0" {
		t.Fatalf("new matches after statuses %+v", fresh)
	}
	e.do("GET", "/api/searches/"+sr.ID+"/matches?status=dismissed", e.key, nil, http.StatusOK, &fresh)
	if len(fresh.Matches) != 1 || fresh.Matches[0].ID != ids["v1|2|0"] {
		t.Fatalf("dismissed %+v", fresh)
	}
	e.do("GET", "/api/searches/"+sr.ID+"/matches?status=archived", e.key, nil, http.StatusUnprocessableEntity, nil)
	e.do("GET", "/api/searches/nope/matches", e.key, nil, http.StatusNotFound, nil)

	// A second search finding the jacket shares its listing but not its
	// status.
	var other searches.Search
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "anything arcteryx", "query": "arcteryx"}, http.StatusCreated, &other)
	e.do("POST", "/api/searches/"+other.ID+"/run", e.key, nil, http.StatusOK, &run)
	if run.Run.NewCount != 3 {
		t.Fatalf("second search's first run %+v", run)
	}
	var detail listings.Detail
	e.do("GET", "/api/listings/"+m0.Listing.ID, e.key, nil, http.StatusOK, &detail)
	if detail.Listing.TotalCents != 12000 || len(detail.PriceHistory) != 2 ||
		detail.PriceHistory[0].TotalCents != 12000 || detail.PriceHistory[1].TotalCents != 14999 {
		t.Fatalf("price history %+v", detail.PriceHistory)
	}
	status := map[string]string{}
	for _, m := range detail.Matches {
		status[m.SearchName] = m.Status
	}
	if len(detail.Matches) != 2 || status["rain shell"] != "seen" || status["anything arcteryx"] != "new" {
		t.Fatalf("found by %+v", detail.Matches)
	}
	e.do("GET", "/api/listings/nope", e.key, nil, http.StatusNotFound, nil)

	// eBay failing is a 502, and the run is logged with its error.
	e.ebay.Fail = http.StatusInternalServerError
	resp := e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusBadGateway, nil)
	_ = resp
	e.ebay.Fail = 0
	var log struct{ Runs []runs.Run }
	e.do("GET", "/api/runs?searchId="+sr.ID, e.key, nil, http.StatusOK, &log)
	if len(log.Runs) != 5 || !strings.Contains(log.Runs[0].Error, "500") || log.Runs[0].FinishedAt == nil || log.Runs[1].Error != "" {
		t.Fatalf("run log %+v", log.Runs)
	}
	e.do("GET", "/api/runs", e.key, nil, http.StatusOK, &log)
	if len(log.Runs) != 6 {
		t.Fatalf("all runs: %d", len(log.Runs))
	}
	e.do("GET", "/api/runs?limit=999", e.key, nil, http.StatusUnprocessableEntity, nil)
	e.do("POST", "/api/searches/nope/run", e.key, nil, http.StatusNotFound, nil)

	// A deleted search's runs leave the log.
	e.do("DELETE", "/api/searches/"+other.ID, e.key, nil, http.StatusNoContent, nil)
	e.do("GET", "/api/runs", e.key, nil, http.StatusOK, &log)
	if len(log.Runs) != 5 {
		t.Fatalf("runs after deleting a search: %d", len(log.Runs))
	}
}

func TestSearchEbayStoresNothing(t *testing.T) {
	e := newEnv(t)
	e.ebay.Items = []map[string]any{ebaytest.Item("v1|9|0", "Beta", "80.00")}

	var res listings.AdHoc
	e.do("GET", "/api/ebay/search?q=arcteryx&conditions=used&maxPriceCents=10000", e.key, nil, http.StatusOK, &res)
	if res.Total != 1 || len(res.Listings) != 1 || res.Listings[0].ID != "" || res.Listings[0].PageURL != "" || res.Listings[0].TotalCents != 8000 {
		t.Fatalf("ad hoc %+v", res)
	}
	q := e.ebay.Searches()[0]
	if q.Get("limit") != "50" || q.Get("filter") != "buyingOptions:{AUCTION|BEST_OFFER|FIXED_PRICE},conditions:{USED},price:[..100.00],priceCurrency:USD" {
		t.Fatalf("sent %v", q)
	}
	e.do("GET", "/api/ebay/search?q=x&conditions=MINT", e.key, nil, http.StatusUnprocessableEntity, nil)
	e.do("GET", "/api/ebay/search?q=%20", e.key, nil, http.StatusUnprocessableEntity, nil)
	if n := len(e.ebay.Searches()); n != 1 {
		t.Fatalf("a refused query still reached eBay: %d requests", n)
	}

	var log struct{ Runs []runs.Run }
	e.do("GET", "/api/runs", e.key, nil, http.StatusOK, &log)
	if len(log.Runs) != 0 {
		t.Fatal("an ad-hoc search logged a run")
	}

	// Without eBay credentials the app serves, and says why it cannot search.
	h := web.New(e.store, ebay.New(ebay.Config{}), e.url).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/ebay/search?q=x", nil)
	req.Header.Set("Authorization", "Bearer "+e.key)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "not configured") {
		t.Fatalf("unconfigured: %d %s", rec.Code, rec.Body)
	}
}
