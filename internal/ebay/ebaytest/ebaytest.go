// Package ebaytest is a fake eBay for tests: the token endpoint and the
// Browse API's item_summary/search, serving items the test sets, with the
// response shapes eBay uses. Point an ebay.Client at Server.URL.
package ebaytest

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
)

// Credentials the fake accepts.
const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
)

// Server is a running fake.
type Server struct {
	*httptest.Server

	mu sync.Mutex
	// Items are the item summaries search pages through, in order. Build
	// them with Item, or write the map by hand for odd shapes.
	Items []map[string]any
	// ExpiresIn is the token lifetime it reports, in seconds (default 7200).
	ExpiresIn int
	// Fail, when non-zero, answers every search with this status and an
	// eBay-style error body.
	Fail int
	// RevokeNext makes the next search answer 401, as when a token is
	// revoked before it expires.
	RevokeNext bool

	tokens   []string
	searches []url.Values
}

// New starts a fake, closed when t ends.
func New(t testing.TB) *Server {
	s := &Server{ExpiresIn: 7200}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /identity/v1/oauth2/token", s.token)
	mux.HandleFunc("GET /buy/browse/v1/item_summary/search", s.search)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// Item is an item summary as eBay shapes one: a fixed-price listing at
// price (a decimal string like "45.00") with free shipping. Change the map
// for anything else.
func Item(id, title, price string) map[string]any {
	return map[string]any{
		"itemId":        id,
		"title":         title,
		"itemWebUrl":    "https://www.ebay.com/itm/" + id,
		"image":         map[string]any{"imageUrl": "https://i.ebayimg.com/images/g/" + id + "/s-l225.jpg"},
		"seller":        map[string]any{"username": "seller_" + id, "feedbackPercentage": "99.8", "feedbackScore": 1234},
		"condition":     "Pre-owned",
		"conditionId":   "3000",
		"buyingOptions": []string{"FIXED_PRICE"},
		"price":         map[string]any{"value": price, "currency": "USD"},
		"shippingOptions": []map[string]any{
			{"shippingCostType": "FIXED", "shippingCost": map[string]any{"value": "0.00", "currency": "USD"}},
		},
		"itemEndDate": "2026-11-20T18:00:00.000Z",
	}
}

// Tokens is how many tokens it has minted.
func (s *Server) Tokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.tokens)
}

// Searches is the query of every search request, in order.
func (s *Server) Searches() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.searches...)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(ClientID+":"+ClientSecret))
	if r.Header.Get("Authorization") != want {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_client", "error_description": "client authentication failed"})
		return
	}
	if r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || r.FormValue("grant_type") != "client_credentials" || r.FormValue("scope") == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request", "error_description": "bad grant"})
		return
	}
	s.mu.Lock()
	tok := "v^1.1#fake-token-" + strconv.Itoa(len(s.tokens)+1)
	s.tokens = append(s.tokens, tok)
	exp := s.ExpiresIn
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": exp, "token_type": "Application Access Token"})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q := r.URL.Query()
	s.searches = append(s.searches, q)

	live := len(s.tokens) > 0 && r.Header.Get("Authorization") == "Bearer "+s.tokens[len(s.tokens)-1]
	if !live || s.RevokeNext {
		s.RevokeNext = false
		writeJSON(w, http.StatusUnauthorized, ebayError(1001, "Invalid access token"))
		return
	}
	if r.Header.Get("X-EBAY-C-MARKETPLACE-ID") == "" {
		writeJSON(w, http.StatusBadRequest, ebayError(12001, "The marketplace header is missing"))
		return
	}
	if s.Fail != 0 {
		writeJSON(w, s.Fail, ebayError(12000, "A system error has occurred"))
		return
	}

	// category_ids narrows to items whose categories include it.
	items := s.Items
	if cat := q.Get("category_ids"); cat != "" {
		items = nil
		for _, it := range s.Items {
			if cats, _ := it["categories"].([]map[string]any); hasCategory(cats, cat) {
				items = append(items, it)
			}
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	end := min(offset+limit, len(items))
	page := []map[string]any{}
	if offset < len(items) {
		page = items[offset:end]
	}
	resp := map[string]any{"total": len(items), "limit": limit, "offset": offset}
	if len(page) > 0 {
		resp["itemSummaries"] = page // eBay leaves the key out when there are none
	}
	if end < len(items) {
		next := *r.URL
		nq := next.Query()
		nq.Set("offset", strconv.Itoa(end))
		next.RawQuery = nq.Encode()
		resp["next"] = s.URL + next.RequestURI()
	}
	writeJSON(w, http.StatusOK, resp)
}

func hasCategory(cats []map[string]any, id string) bool {
	for _, c := range cats {
		if c["categoryId"] == id {
			return true
		}
	}
	return false
}

func ebayError(id int, msg string) map[string]any {
	return map[string]any{"errors": []map[string]any{{
		"errorId": id, "domain": "API_BROWSE", "category": "REQUEST", "message": msg, "longMessage": msg + ".",
	}}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Categories sets an item's categories, for category_ids filtering.
func Categories(item map[string]any, ids ...string) map[string]any {
	cats := make([]map[string]any, len(ids))
	for i, id := range ids {
		cats[i] = map[string]any{"categoryId": id}
	}
	item["categories"] = cats
	return item
}
