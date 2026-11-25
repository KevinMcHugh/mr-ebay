package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"

	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server/deletion"
)

// TestAccountDeletion drives eBay's Marketplace Account Deletion contract
// as eBay does, with no credential: the registration challenge, then a
// signed notice that scrubs its seller from every stored listing, the soft-
// deleted ones too, and a forged notice that changes nothing.
func TestAccountDeletion(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	endpoint := e.url + "/api" + deletion.Path

	get := func(code string) (int, string) {
		resp, err := http.Get(endpoint + "?" + url.Values{"challenge_code": {code}}.Encode())
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var body struct {
			ChallengeResponse string `json:"challengeResponse"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body.ChallengeResponse
	}
	post := func(body []byte, sig string) int {
		req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-EBAY-SIGNATURE", sig)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	// The challenge, before and after the token is set.
	if code, _ := get("abc"); code != http.StatusServiceUnavailable {
		t.Fatalf("challenge with no token: %d, want 503", code)
	}
	token := "verify_0123456789abcdef0123456789abcdef"
	e.srv.API.DeletionToken = token
	sum := sha256.Sum256([]byte("abc" + token + endpoint))
	if code, got := get("abc"); code != http.StatusOK || got != hex.EncodeToString(sum[:]) {
		t.Fatalf("challenge: %d %q, want 200 %s", code, got, hex.EncodeToString(sum[:]))
	}

	// Store two listings from two sellers, and soft-delete a third from the
	// first seller, as a deleted search would leave it.
	gone := ebaytest.Item("v1|1|0", "Beta jacket", "45.00")
	other := ebaytest.Item("v1|2|0", "Gamma pants", "30.00")
	e.ebay.Items = []map[string]any{gone, other}
	var sr struct{ ID string }
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "shell", "query": "arcteryx"}, http.StatusCreated, &sr)
	e.do("POST", "/api/searches/"+sr.ID+"/run", e.key, nil, http.StatusOK, nil)
	seller := "seller_v1|1|0"
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO listings (id, ebay_item_id, marketplace, title, url, seller, seller_feedback_pct, price_cents, currency, data, deleted_at)
		VALUES ('deletedlisting000000', 'v1|9|0', 'EBAY_US', 'Old', 'https://ebay.example/9', $1, 99.1, 100, 'USD', '{"seller":{"username":"x"}}', NOW())`, seller); err != nil {
		t.Fatal(err)
	}

	type row struct {
		Seller    string
		Feedback  *float64
		HasSeller bool
	}
	read := func(item string) row {
		var r row
		if err := e.store.Pool.QueryRow(ctx, `SELECT seller, seller_feedback_pct, data ? 'seller' FROM listings WHERE ebay_item_id = $1`, item).
			Scan(&r.Seller, &r.Feedback, &r.HasSeller); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := read("v1|1|0"); r.Seller != seller || r.Feedback == nil || !r.HasSeller {
		t.Fatalf("stored listing %+v", r)
	}

	notice, _ := json.Marshal(map[string]any{
		"metadata":     map[string]any{"topic": deletion.Topic, "schemaVersion": "1.0", "deprecated": false},
		"notification": map[string]any{"notificationId": "n-1", "publishAttemptCount": 1, "data": map[string]any{"username": seller, "userId": "u-1", "eiasToken": "e-1"}},
	})

	// A forged notice: signed, but over a different body.
	if code := post(notice, e.ebay.Sign([]byte(`{"metadata":{}}`))); code != http.StatusPreconditionFailed {
		t.Fatalf("forged notice: %d, want 412", code)
	}
	if r := read("v1|1|0"); r.Seller != seller {
		t.Fatalf("a forged notice changed the listing: %+v", r)
	}

	// The real one.
	if code := post(notice, e.ebay.Sign(notice)); code != http.StatusNoContent {
		t.Fatalf("notice: %d, want 204", code)
	}
	for _, item := range []string{"v1|1|0", "v1|9|0"} {
		if r := read(item); r.Seller != "" || r.Feedback != nil || r.HasSeller {
			t.Errorf("%s still has its seller: %+v", item, r)
		}
	}
	if r := read("v1|2|0"); r.Seller != "seller_v1|2|0" || !r.HasSeller {
		t.Errorf("another seller's listing changed: %+v", r)
	}
	// The listing itself is still there, and still reads through the API.
	var l struct {
		Listing struct{ Title, Seller string }
	}
	var id string
	if err := e.store.Pool.QueryRow(ctx, `SELECT id FROM listings WHERE ebay_item_id = 'v1|1|0'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/api/listings/"+id, e.key, nil, http.StatusOK, &l)
	if l.Listing.Title != "Beta jacket" || l.Listing.Seller != "" {
		t.Errorf("listing after deletion %+v", l.Listing)
	}

	// eBay sends a notice again if the first delivery is not acknowledged.
	if code := post(notice, e.ebay.Sign(notice)); code != http.StatusNoContent {
		t.Fatalf("repeated notice: %d, want 204", code)
	}
}
