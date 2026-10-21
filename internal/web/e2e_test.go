package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/rs/xid"

	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/auth"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay/ebaytest"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/runs"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
	"github.com/KevinMcHugh/mr-ebay/internal/web"
)

// The end-to-end tests need a Postgres they may wipe: point
// TEST_DATABASE_URL at a scratch database. Without it they skip, so
// `go test ./...` passes on a machine with no Postgres.
const dbEnv = "TEST_DATABASE_URL"

type env struct {
	t      *testing.T
	url    string
	ebay   *ebaytest.Server
	store  *app.Store
	key    string
	keyID  string
	q      *dbgen.Queries
	client *http.Client // never follows redirects
}

func newEnv(t *testing.T) *env {
	dsn := os.Getenv(dbEnv)
	if dsn == "" {
		t.Skip(dbEnv + " not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrate(t, pool)

	ts := httptest.NewServer(nil)
	t.Cleanup(ts.Close)
	store := app.NewStore(pool)
	fake := ebaytest.New(t)
	eb := ebay.New(ebay.Config{ClientID: ebaytest.ClientID, ClientSecret: ebaytest.ClientSecret, BaseURL: fake.URL})
	ts.Config.Handler = web.New(store, eb, ts.URL).Handler()

	raw, err := auth.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := store.CreateAPIKey(ctx, dbgen.CreateAPIKeyParams{ID: xid.New().String(), Name: "test", KeyHash: auth.Hash(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, url: ts.URL, ebay: fake, store: store, key: raw, keyID: key.ID, q: store.Queries, client: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// migrate rebuilds the schema from the dbmate files' up sections.
func migrate(t *testing.T, pool *pgxpool.Pool) {
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob("../../db/migrations/*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("no migrations found: %v", err)
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		up, _, _ := strings.Cut(string(b), "-- migrate:down")
		if _, err := pool.Exec(ctx, up); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// do sends a request with a bearer token (if any) and decodes a JSON reply
// into out (if non-nil), failing unless the status is want.
func (e *env) do(method, path, token string, body any, want int, out any) *http.Response {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.url+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			e.t.Fatalf("%s %s: decode %s: %v", method, path, data, err)
		}
	}
	return resp
}

func TestAPIFlow(t *testing.T) {
	e := newEnv(t)

	resp := e.do("GET", "/api/me", "", nil, http.StatusUnauthorized, nil)
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource") {
		t.Fatalf("401 does not point at OAuth discovery: %q", resp.Header.Get("WWW-Authenticate"))
	}
	e.do("GET", "/api/me", auth.KeyPrefix+"not-a-key", nil, http.StatusUnauthorized, nil)
	e.do("POST", "/api/searches", "", map[string]any{"name": "x", "query": "y"}, http.StatusUnauthorized, nil)
	e.do("GET", "/api/searches", "", nil, http.StatusUnauthorized, nil)

	var me struct{ KeyID, Name string }
	e.do("GET", "/api/me", e.key, nil, http.StatusOK, &me)
	if me.KeyID != e.keyID || me.Name != "test" {
		t.Fatalf("me %+v", me)
	}

	var sr searches.Search
	e.do("POST", "/api/searches", e.key, map[string]any{
		"name": " rain shell ", "query": "arcteryx beta", "conditions": []string{"used"},
		"maxPriceCents": 20000, "wants": "size M", "watch": true,
	}, http.StatusCreated, &sr)
	if sr.Name != "rain shell" || sr.Currency != "USD" || sr.Marketplace != "EBAY_US" || sr.MaxPriceCents != 20000 ||
		len(sr.Conditions) != 1 || sr.Conditions[0] != "USED" || !sr.Watch || sr.LastRunAt != nil || sr.URL != e.url+"/searches/"+sr.ID {
		t.Fatalf("created %+v", sr)
	}
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "x", "query": " "}, http.StatusUnprocessableEntity, nil)
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "x", "query": "y", "conditions": []string{"MINT"}}, http.StatusUnprocessableEntity, nil)
	e.do("POST", "/api/searches", e.key, map[string]any{"name": "x", "query": "y", "minPriceCents": 500, "maxPriceCents": 100}, http.StatusUnprocessableEntity, nil)

	// A PATCH changes only what it sends, and a price of 0 removes the limit.
	var upd searches.Search
	e.do("PATCH", "/api/searches/"+sr.ID, e.key, map[string]any{"maxPriceCents": 0, "minPriceCents": 5000}, http.StatusOK, &upd)
	if upd.MaxPriceCents != 0 || upd.MinPriceCents != 5000 || upd.Query != "arcteryx beta" || upd.Wants != "size M" || !upd.Watch {
		t.Fatalf("updated %+v", upd)
	}
	e.do("PATCH", "/api/searches/"+sr.ID, e.key, map[string]any{"maxPriceCents": 100}, http.StatusUnprocessableEntity, nil)
	e.do("PATCH", "/api/searches/nope", e.key, map[string]any{"wants": "x"}, http.StatusNotFound, nil)

	var list struct{ Searches []searches.Search }
	e.do("GET", "/api/searches", e.key, nil, http.StatusOK, &list)
	if len(list.Searches) != 1 || list.Searches[0].ID != sr.ID || list.Searches[0].NewCount != 0 {
		t.Fatalf("list %+v", list)
	}

	// Delete is soft, and a second one is a 404.
	e.do("DELETE", "/api/searches/"+sr.ID, e.key, nil, http.StatusNoContent, nil)
	e.do("DELETE", "/api/searches/"+sr.ID, e.key, nil, http.StatusNotFound, nil)
	e.do("GET", "/api/searches/"+sr.ID, e.key, nil, http.StatusNotFound, nil)
}

// pkce is a verifier and its S256 challenge.
func pkce() (verifier, challenge string) {
	verifier = "a-verifier-that-is-long-enough-for-pkce-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestOAuthFlow(t *testing.T) {
	e := newEnv(t)
	const redirect = "https://claude.example/callback"

	var reg struct {
		ClientID string `json:"client_id"`
	}
	e.do("POST", "/oauth/register", "", map[string]any{"redirect_uris": []string{redirect}, "client_name": "test"}, http.StatusCreated, &reg)

	verifier, challenge := pkce()
	form := url.Values{
		"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
		"state": {"xyz"}, "code_challenge": {challenge}, "code_challenge_method": {"S256"},
	}
	resp, err := e.client.Get(e.url + "/oauth/authorize?" + form.Encode())
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize page: %v %v", resp.StatusCode, err)
	}
	form.Set("api_key", auth.KeyPrefix+"wrong")
	if resp, _ = e.client.PostForm(e.url+"/oauth/authorize", form); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a wrong key should not authorize: %d", resp.StatusCode)
	}
	form.Set("api_key", e.key)
	resp, err = e.client.PostForm(e.url+"/oauth/authorize", form)
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize: %v %v", resp.StatusCode, err)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("state") != "xyz" || loc.Query().Get("code") == "" {
		t.Fatalf("redirect %s", loc)
	}

	token := func(v url.Values, want int) map[string]any {
		t.Helper()
		resp, err := e.client.PostForm(e.url+"/oauth/token", v)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		if resp.StatusCode != want {
			t.Fatalf("token: %d %v, want %d", resp.StatusCode, out, want)
		}
		return out
	}
	codeGrant := url.Values{"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"code_verifier": {verifier}, "client_id": {reg.ClientID}, "redirect_uri": {redirect}}
	tok := token(codeGrant, http.StatusOK)
	token(codeGrant, http.StatusBadRequest) // a code works once

	access, refresh := tok["access_token"].(string), tok["refresh_token"].(string)
	e.do("GET", "/api/me", access, nil, http.StatusOK, nil)

	refreshGrant := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}}
	next := token(refreshGrant, http.StatusOK)
	token(refreshGrant, http.StatusBadRequest) // a refresh token works once
	e.do("GET", "/api/me", access, nil, http.StatusUnauthorized, nil)
	e.do("GET", "/api/me", next["access_token"].(string), nil, http.StatusOK, nil)

	// Deleting the key cuts off every token it approved.
	if err := e.q.DeleteAPIKey(context.Background(), e.keyID); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/api/me", next["access_token"].(string), nil, http.StatusUnauthorized, nil)
}

func TestInviteFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	const redirect = "https://claude.example/callback"
	var reg struct {
		ClientID string `json:"client_id"`
	}
	e.do("POST", "/oauth/register", "", map[string]any{"redirect_uris": []string{redirect}}, http.StatusCreated, &reg)

	invite := func(note string, expires time.Duration) string {
		t.Helper()
		raw, _ := auth.RandToken(auth.InvitePrefix)
		arg := dbgen.CreateInviteParams{ID: xid.New().String(), CodeHash: auth.Hash(raw), Note: note}
		if expires != 0 {
			arg.ExpiresAt = pgtype.Timestamptz{Time: time.Now().Add(expires), Valid: true}
		}
		if _, err := e.q.CreateInvite(ctx, arg); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	verifier, challenge := pkce()
	// authorize pastes credential on the authorize page and returns the
	// status and, on success, the code.
	authorize := func(credential string) (int, string) {
		t.Helper()
		form := url.Values{
			"response_type": {"code"}, "client_id": {reg.ClientID}, "redirect_uri": {redirect},
			"code_challenge": {challenge}, "code_challenge_method": {"S256"},
			"api_key": {credential},
		}
		resp, err := e.client.PostForm(e.url+"/oauth/authorize", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc, _ := url.Parse(resp.Header.Get("Location"))
		return resp.StatusCode, loc.Query().Get("code")
	}

	raw := invite("ana", 0)
	status, code := authorize(raw)
	if status != http.StatusSeeOther || code == "" {
		t.Fatalf("redeem invite: %d", status)
	}
	if status, _ = authorize(raw); status != http.StatusBadRequest {
		t.Fatalf("an invite works once: %d", status)
	}
	if status, _ = authorize(auth.InvitePrefix + "not-an-invite"); status != http.StatusBadRequest {
		t.Fatalf("unknown invite: %d", status)
	}
	if status, _ = authorize(invite("late", -time.Minute)); status != http.StatusBadRequest {
		t.Fatalf("expired invite: %d", status)
	}

	resp, err := e.client.PostForm(e.url+"/oauth/token", url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"code_verifier": {verifier}, "client_id": {reg.ClientID}, "redirect_uri": {redirect}})
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("token: %v %v", resp.StatusCode, err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()

	// The invite minted a key named after its note, and the token acts as it.
	var me struct{ KeyID, Name string }
	e.do("GET", "/api/me", tok.AccessToken, nil, http.StatusOK, &me)
	if me.Name != "ana" {
		t.Fatalf("redeeming the invite minted key %+v, want one named ana", me)
	}
	// The newcomer's key is a key like any other: deleting it cuts them off.
	if err := e.q.DeleteAPIKey(ctx, me.KeyID); err != nil {
		t.Fatal(err)
	}
	e.do("GET", "/api/me", tok.AccessToken, nil, http.StatusUnauthorized, nil)
}

func TestWebPages(t *testing.T) {
	e := newEnv(t)
	jar, _ := cookiejar.New(nil)
	browser := &http.Client{Jar: jar, CheckRedirect: e.client.CheckRedirect}

	page := func(path string) string {
		t.Helper()
		resp, err := browser.Get(e.url + path)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: %v %v", path, resp.StatusCode, err)
		}
		if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Fatalf("GET %s: no CSP", path)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	// Signed out, every page sends the browser to sign in, and writes bounce.
	resp, _ := browser.Get(e.url + "/")
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("signed-out index: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = browser.PostForm(e.url+"/searches", url.Values{"name": {"x"}, "query": {"y"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed-out create: %d", resp.StatusCode)
	}
	resp, _ = browser.PostForm(e.url+"/login", url.Values{"api_key": {"wrong"}, "next": {"/"}})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad login: %d", resp.StatusCode)
	}
	resp, _ = browser.PostForm(e.url+"/login", url.Values{"api_key": {e.key}, "next": {"//evil.example"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d %s (next must stay on this site)", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp, _ = browser.PostForm(e.url+"/searches", url.Values{
		"name": {"Rain shell"}, "query": {"arcteryx beta"}, "maxPrice": {"$200"}, "minPrice": {"12.5"},
		"conditions": {"USED"}, "buyingOptions": {"FIXED_PRICE", "BEST_OFFER"}, "categoryIds": {"57988, 11450"},
		"currency": {"USD"}, "marketplace": {"EBAY_US"}, "wants": {"size M"}, "watch": {"1"},
	})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("create: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	searchPath, _, _ := strings.Cut(resp.Header.Get("Location"), "?")
	searchID := strings.TrimPrefix(searchPath, "/searches/")
	if body := page("/"); !strings.Contains(body, "Rain shell") || !strings.Contains(body, ">"+searchID+"</code>") || !strings.Contains(body, "watching") {
		t.Fatal("index does not list the search with its id")
	}
	detail := page(searchPath)
	for _, want := range []string{"from 12.50 USD", "up to 200.00 USD", "BEST_OFFER, FIXED_PRICE", "11450, 57988", `value="12.50"`, "size M", "Sign out"} {
		if !strings.Contains(detail, want) {
			t.Errorf("search page lacks %q", want)
		}
	}
	resp, _ = browser.PostForm(e.url+"/searches", url.Values{"name": {"x"}, "query": {"y"}, "maxPrice": {"1.234"}})
	if loc := resp.Header.Get("Location"); resp.StatusCode != http.StatusSeeOther || !strings.Contains(loc, "err=") {
		t.Fatalf("a bad amount should come back as a message: %d %s", resp.StatusCode, loc)
	}

	// The edit form posts every field; clearing max price removes the limit.
	resp, _ = browser.PostForm(e.url+searchPath, url.Values{"name": {"Rain shell"}, "query": {"arcteryx beta"}, "maxPrice": {""}})
	if resp.StatusCode != http.StatusSeeOther || strings.Contains(resp.Header.Get("Location"), "err=") {
		t.Fatalf("update: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if body := page(searchPath); strings.Contains(body, "up to") || strings.Contains(body, "watching") {
		t.Error("update did not clear the max price and watch")
	}

	// A cross-site form post is refused even with the cookie.
	req, _ := http.NewRequest("POST", e.url+searchPath+"/delete", nil)
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ = browser.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin delete: %d", resp.StatusCode)
	}
	resp, _ = browser.PostForm(e.url+searchPath+"/delete", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: %d", resp.StatusCode)
	}

	browser.PostForm(e.url+"/logout", nil) //nolint:errcheck
	if resp, _ = browser.Get(e.url + "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("signed out but the index still opens")
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func TestMCPTools(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcpsdk.StreamableClientTransport{
		Endpoint:   e.url + "/mcp/rpc",
		HTTPClient: &http.Client{Transport: bearer{e.key, http.DefaultTransport}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	call := func(name string, args map[string]any, out any) {
		t.Helper()
		res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if res.IsError {
			t.Fatalf("%s: tool error %+v", name, res.Content)
		}
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	var who struct{ Me struct{ KeyID, Name string } }
	call("whoami", nil, &who)
	if who.Me.KeyID != e.keyID {
		t.Fatalf("whoami %+v", who)
	}

	var created struct{ Search searches.Search }
	call("create_search", map[string]any{"name": "Rain shell", "query": "arcteryx beta", "maxPriceCents": 20000, "wants": "size M"}, &created)
	var updated struct{ Search searches.Search }
	call("update_search", map[string]any{"id": created.Search.ID, "watch": true, "maxPriceCents": 0}, &updated)
	if !updated.Search.Watch || updated.Search.MaxPriceCents != 0 || updated.Search.Wants != "size M" {
		t.Fatalf("update_search: %+v", updated.Search)
	}
	var list struct{ Searches []searches.Search }
	call("list_searches", nil, &list)
	if len(list.Searches) != 1 {
		t.Fatalf("list_searches: %+v", list)
	}
	res, err := session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "create_search", Arguments: map[string]any{"name": "x", "query": "y", "conditions": []string{"MINT"}}})
	if err != nil || !res.IsError {
		t.Fatalf("a bad condition should come back as a tool error: %v %+v", err, res)
	}
	var ok struct{ OK bool }
	call("delete_search", map[string]any{"id": created.Search.ID}, &ok)

	res, err = session.CallTool(ctx, &mcpsdk.CallToolParams{Name: "get_search", Arguments: map[string]any{"id": created.Search.ID}})
	if err != nil || !res.IsError {
		t.Fatalf("a deleted search should come back as a tool error: %v %+v", err, res)
	}

	// The eBay tools, against the fake.
	e.ebay.Items = []map[string]any{ebaytest.Item("v1|5|0", "Beta jacket", "90.00")}
	var adhoc struct{ Total int }
	call("search_ebay", map[string]any{"query": "beta"}, &adhoc)
	if adhoc.Total != 1 {
		t.Fatalf("search_ebay %+v", adhoc)
	}
	call("create_search", map[string]any{"name": "Beta", "query": "beta"}, &created)
	var ran runs.Result
	call("run_search", map[string]any{"id": created.Search.ID}, &ran)
	if ran.Run.NewCount != 1 || len(ran.NewMatches) != 1 || ran.PriceDrops == nil {
		t.Fatalf("run_search %+v", ran)
	}
	m := ran.NewMatches[0]
	var set struct{ Updated int64 }
	call("set_match_status", map[string]any{"ids": []string{m.ID}, "status": "starred"}, &set)
	var starred struct{ Matches []matches.Match }
	call("list_matches", map[string]any{"searchId": created.Search.ID, "status": "starred"}, &starred)
	if set.Updated != 1 || len(starred.Matches) != 1 {
		t.Fatalf("starred %+v", starred)
	}
	var listing struct{ Listing listings.Detail }
	call("get_listing", map[string]any{"id": m.Listing.ID}, &listing)
	if len(listing.Listing.PriceHistory) != 1 || listing.Listing.Matches[0].Status != "starred" {
		t.Fatalf("get_listing %+v", listing.Listing)
	}
	var runLog struct{ Runs []runs.Run }
	call("list_runs", map[string]any{"searchId": created.Search.ID}, &runLog)
	if len(runLog.Runs) != 1 {
		t.Fatalf("list_runs %+v", runLog)
	}

	// Without a credential, MCP is a 401 pointing at OAuth discovery.
	resp, err := http.Post(e.url+"/mcp/rpc", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatalf("anonymous MCP: %d", resp.StatusCode)
	}
}
