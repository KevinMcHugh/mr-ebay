package web_test

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"

	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// TestMatchPages drives the search page's match grid, the run button, the
// status buttons and the listing page as a signed-in browser would.
func TestMatchPages(t *testing.T) {
	e := newEnv(t)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: e.client.CheckRedirect}
	resp, _ := browser.PostForm(e.url+"/login", url.Values{"api_key": {e.key}, "next": {"/"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	page := func(path string) string {
		t.Helper()
		resp, err := browser.Get(e.url + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp.StatusCode, err)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "img-src 'self' data: https://i.ebayimg.com") {
			t.Fatalf("GET %s: CSP %q", path, csp)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	// post submits a form and returns where it redirected to.
	post := func(path string, form url.Values) *url.URL {
		t.Helper()
		resp, err := browser.PostForm(e.url+path, form)
		if err != nil || resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST %s: %v %v", path, resp.StatusCode, err)
		}
		loc, _ := url.Parse(resp.Header.Get("Location"))
		return loc
	}

	var sr searches.Search
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "rain shell", "query": "arcteryx beta"}, http.StatusCreated, &sr)
	searchPath := "/searches/" + sr.ID
	if body := page(searchPath); !strings.Contains(body, "has not run yet") || !strings.Contains(body, "Run now") {
		t.Fatal("an unrun search should say so and offer to run")
	}

	jacket := ebaytest.Item("v1|1|0", "Arcteryx Beta jacket M", "149.99")
	e.ebay.Items = []map[string]any{jacket, ebaytest.Item("v1|2|0", "Beta AR <b>bold</b>", "80.00")}
	loc := post(searchPath+"/run", nil)
	if loc.Path != searchPath || loc.Query().Get("status") != "new" || loc.Query().Get("msg") != "Found 2 listings: 2 new, 0 price drops." {
		t.Fatalf("run redirected to %s", loc)
	}
	body := page(loc.RequestURI())
	for _, want := range []string{
		"Arcteryx Beta jacket M", "149.99 USD", "free shipping", "seller_v1|1|0 (99.8%)", "ends Nov 20",
		`src="https://i.ebayimg.com/images/g/`, `href="https://www.ebay.com/itm/`, `aria-current="page">new (2)`,
		"Mark these 2 seen", "Found 2 listings", "Recent runs",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("search page lacks %q", want)
		}
	}
	// A seller's title is text, never markup.
	if strings.Contains(body, "<b>bold</b>") || !strings.Contains(body, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Error("listing title was not escaped")
	}

	var ms struct{ Matches []matches.Match }
	e.do("GET", "/api"+searchPath+"/matches", e.key, nil, http.StatusOK, &ms)
	ids := map[string]matches.Match{}
	for _, m := range ms.Matches {
		ids[m.Listing.EbayItemID] = m
	}
	jacketMatch, other := ids["v1|1|0"], ids["v1|2|0"]

	// Dismiss one from its card, and come back to the same tab.
	back := searchPath + "?status=new"
	loc = post("/matches/status", url.Values{"ids": {other.ID}, "status": {"dismissed"}, "back": {back}})
	if loc.Path != searchPath || loc.Query().Get("status") != "new" || loc.Query().Get("msg") != "Marked 1 dismissed." {
		t.Fatalf("status redirected to %s", loc)
	}
	if body = page(back); strings.Contains(body, "Beta AR") || !strings.Contains(body, "new (1)") {
		t.Fatal("the new tab still shows the dismissed match")
	}
	if body = page(searchPath + "?status=dismissed"); !strings.Contains(body, "Beta AR") || !strings.Contains(body, "status-dismissed") {
		t.Fatal("the dismissed tab does not show it")
	}

	// A bad status, or a back link to another site, goes nowhere harmful.
	loc = post("/matches/status", url.Values{"ids": {other.ID}, "status": {"archived"}, "back": {back}})
	if loc.Query().Get("err") == "" {
		t.Fatalf("bad status: %s", loc)
	}
	if loc = post("/matches/status", url.Values{"ids": {other.ID}, "status": {"seen"}, "back": {"//evil.example/x"}}); loc.Host != "" || loc.Path != "/" {
		t.Fatalf("back left the site: %s", loc)
	}

	// With nothing new, the page opens on every match.
	post("/matches/status", url.Values{"ids": {jacketMatch.ID}, "status": {"starred"}, "back": {searchPath}})
	if body = page(searchPath); !strings.Contains(body, `aria-current="page">all`) || !strings.Contains(body, "status-starred") {
		t.Fatal("with nothing new the page should open on all matches")
	}

	// The listing page: photo, price history, and the searches that found it.
	jacket["price"] = map[string]any{"value": "120.00", "currency": "USD"}
	loc = post(searchPath+"/run", nil)
	if !strings.Contains(loc.Query().Get("msg"), "1 price drop.") {
		t.Fatalf("second run: %s", loc)
	}
	body = page("/listings/" + jacketMatch.Listing.ID)
	for _, want := range []string{"Arcteryx Beta jacket M", "120.00 USD", "149.99 USD", "rain shell", "starred", "Open on eBay", "v1|1|0", "Mark new"} {
		if !strings.Contains(body, want) {
			t.Errorf("listing page lacks %q", want)
		}
	}
	if resp, _ := browser.Get(e.url + "/listings/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing listing: %d", resp.StatusCode)
	}

	// eBay failing is a message on the page and a row in the run log.
	e.ebay.Fail = http.StatusInternalServerError
	loc = post(searchPath+"/run", nil)
	if !strings.Contains(loc.Query().Get("err"), "eBay unavailable") {
		t.Fatalf("failed run: %s", loc)
	}
	if body = page(loc.RequestURI()); !strings.Contains(body, `<tr class="err">`) {
		t.Error("the run log does not show the failed run")
	}

	// Signed out, none of it opens.
	browser.PostForm(e.url+"/logout", nil) //nolint:errcheck
	if resp, _ := browser.Get(e.url + "/listings/" + jacketMatch.Listing.ID); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signed-out listing page: %d", resp.StatusCode)
	}
	if resp, _ := browser.PostForm(e.url+"/matches/status", url.Values{"ids": {other.ID}, "status": {"seen"}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed-out status change: %d", resp.StatusCode)
	}
}
