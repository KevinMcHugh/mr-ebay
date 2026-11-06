package web_test

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// TestPagination walks every list a page at a time and checks the pages
// add up to the whole list, newest first, with nothing twice; that every
// list refuses a bad limit or cursor; and that the pages link from one page
// of matches to the next.
func TestPagination(t *testing.T) {
	e := newEnv(t)
	for i := range 105 {
		e.ebay.Items = append(e.ebay.Items, ebaytest.Item(fmt.Sprintf("v1|%d|0", i), fmt.Sprint("Item ", i), "10.00"))
	}
	var sr searches.Search
	for i := range 5 {
		e.do("POST", "/api/searches", e.key, map[string]any{"name": fmt.Sprint("search ", i), "query": "q"}, http.StatusCreated, &sr)
	}
	// Three runs of the last search: 105 matches, all first seen in one
	// transaction, so the id has to break the tie between them.
	for range 3 {
		e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, nil)
	}

	// ids is every id on one page of path's list, and its next.
	ids := func(path, field, limit, after string) ([]string, string) {
		t.Helper()
		q := url.Values{}
		if limit != "" {
			q.Set("limit", limit)
		}
		if after != "" {
			q.Set("after", after)
		}
		sep := "?"
		if strings.Contains(path, "?") {
			sep = "&"
		}
		var body map[string]json.RawMessage
		e.do("GET", path+sep+q.Encode(), e.key, nil, http.StatusOK, &body)
		var items []struct{ ID string }
		if err := json.Unmarshal(body[field], &items); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var next string
		_ = json.Unmarshal(body["next"], &next)
		out := make([]string, len(items))
		for i, it := range items {
			out[i] = it.ID
		}
		return out, next
	}

	for _, l := range []struct {
		path, field string
		count       int
	}{
		{"/api/searches", "searches", 5},
		{"/api/searches/" + sr.ID + "/matches", "matches", 105},
		{"/api/searches/" + sr.ID + "/matches?status=new", "matches", 105},
		{"/api/runs", "runs", 3},
		{"/api/runs?searchId=" + sr.ID, "runs", 3},
	} {
		whole, next := ids(l.path, l.field, "200", "")
		if len(whole) != l.count || next != "" {
			t.Fatalf("%s: one page of %d, want %d; next %q", l.path, len(whole), l.count, next)
		}
		var paged []string
		after := ""
		for range len(whole) {
			page, next := ids(l.path, l.field, "2", after)
			if len(page) > 2 {
				t.Fatalf("%s: a page of %d for a limit of 2", l.path, len(page))
			}
			paged = append(paged, page...)
			if next == "" {
				break
			}
			after = next
		}
		if !slices.Equal(paged, whole) {
			t.Errorf("%s: paged %d, want the whole list of %d in order", l.path, len(paged), len(whole))
		}
		sep := "?"
		if strings.Contains(l.path, "?") {
			sep = "&"
		}
		e.do("GET", l.path+sep+"limit=201", e.key, nil, http.StatusUnprocessableEntity, nil)
		e.do("GET", l.path+sep+"after=nope", e.key, nil, http.StatusUnprocessableEntity, nil)
		_, other := ids("/api/searches", "searches", "1", "")
		if l.field != "searches" {
			e.do("GET", l.path+sep+"after="+other, e.key, nil, http.StatusUnprocessableEntity, nil)
		}
	}

	// The default page is 100.
	first, next := ids("/api/searches/"+sr.ID+"/matches", "matches", "", "")
	if len(first) != 100 || next == "" {
		t.Fatalf("default page: %d, next %q", len(first), next)
	}

	// The search page shows the first 100 and links to the rest.
	page := func(path string) string {
		t.Helper()
		req, _ := http.NewRequest("GET", e.url+path, nil)
		req.Header.Set("Authorization", "Bearer "+e.key)
		resp, err := e.client.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp.StatusCode, err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}
	cards := regexp.MustCompile(`<h3><a href="[^"]*/listings/`)
	body := page("/searches/" + sr.ID + "?status=all")
	m := regexp.MustCompile(`href="(/searches/[^"]+after=[^"]+)">Older matches`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("the search page has no link to older matches")
	}
	older := page(html.UnescapeString(m[1]))
	if !strings.Contains(older, "Newest matches") || strings.Contains(older, "Older matches") {
		t.Fatal("the older matches page should link back and end the list")
	}
	if a, b := len(cards.FindAllString(body, -1)), len(cards.FindAllString(older, -1)); a != 100 || b != 5 {
		t.Fatalf("the search page showed %d then %d matches, want 100 then 5", a, b)
	}
}
