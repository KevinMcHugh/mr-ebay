package server_test

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/server"
)

// TestPublicOperations pins which operations anyone may call. Searches and
// what they find are personal, so today that is none. Update this only when
// the spec deliberately marks an operation `security: []`.
func TestPublicOperations(t *testing.T) {
	if got := slices.Sorted(maps.Keys(server.PublicOperations())); len(got) != 0 {
		t.Fatalf("public operations %v, want none", got)
	}
}

// nilStore panics on any call, through its nil Querier or Pager or here.
type nilStore struct {
	dbgen.Querier
	app.Pager
}

func (nilStore) RecordResults(context.Context, string, []ebay.Item) ([]app.Recorded, error) {
	panic("reached the store")
}

// TestAnonymousCallers checks every operation refuses a caller with no
// credential before it reaches the store: nilStore and the nil eBay client panic if one does.
func TestAnonymousCallers(t *testing.T) {
	r := chi.NewRouter()
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	server.New(nilStore{}, nil, app.NewLinks("https://app.example")).Mount(r, deny)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/me"},
		{"GET", "/api/searches"},
		{"POST", "/api/searches"},
		{"GET", "/api/searches/s1"},
		{"PATCH", "/api/searches/s1"},
		{"DELETE", "/api/searches/s1"},
		{"POST", "/api/searches/s1/run"},
		{"GET", "/api/searches/s1/matches"},
		{"POST", "/api/matches/status"},
		{"GET", "/api/listings/l1"},
		{"GET", "/api/runs"},
		{"GET", "/api/ebay/search?q=x"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`)))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401: %s", tc.method, tc.path, rec.Code, rec.Body)
		}
		if cors := rec.Header().Get("Access-Control-Allow-Origin"); cors != "" {
			t.Errorf("%s %s: Access-Control-Allow-Origin %q on a private operation", tc.method, tc.path, cors)
		}
	}
}

// TestEveryListIsPaged checks the pagination contract on every list
// operation, including ones added after this test was: it takes limit and
// after, its body carries the next cursor, and it declares the 422 it gets
// for a bad limit or cursor. Server's handlers run them with RunPaged, so
// the endpoint behind each one has to page to compile.
func TestEveryListIsPaged(t *testing.T) {
	spec, err := apigen.GetSwagger()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for path, item := range spec.Paths.Map() {
		op := item.Get
		if op == nil || !strings.HasPrefix(op.OperationID, "List") {
			continue
		}
		n++
		for _, name := range []string{"limit", "after"} {
			if op.Parameters.GetByInAndName("query", name) == nil {
				t.Errorf("GET %s: no %s parameter", path, name)
			}
		}
		body := op.Responses.Status(http.StatusOK).Value.Content.Get("application/json").Schema.Value
		if !slices.Contains(body.Required, "next") {
			t.Errorf("GET %s: the body has no next", path)
		}
		if op.Responses.Status(http.StatusUnprocessableEntity) == nil {
			t.Errorf("GET %s: no 422 response", path)
		}
	}
	if n == 0 {
		t.Fatal("no list operations in the spec")
	}
}

// overlong is a Paged endpoint that ignores the limit it was asked for.
type overlong struct{}

func (overlong) Interact(context.Context, int) (app.Page[int], error) {
	return app.Page[int]{Items: []int{1, 2, 3}}, nil
}
func (overlong) Build(p app.Page[int]) app.Page[int] { return p }
func (overlong) Render(p app.Page[int]) int          { return p.Len() }
func (overlong) Page(limit int) app.PageRequest      { return app.PageRequest{List: "n", Limit: limit} }

// A list endpoint that returns more than the caller asked for fails closed
// on every surface, rather than handing back a large list whole.
func TestOverlongPageFailsClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := server.RunPaged(ctx, overlong{}, 2); err == nil {
		t.Fatal("Run passed a page of 3 for a limit of 2")
	}
	if _, err := server.BuildViewModel(ctx, overlong{}, 2); err == nil {
		t.Fatal("BuildViewModel passed a page of 3 for a limit of 2")
	}
	if n, err := server.RunPaged(ctx, overlong{}, 3); err != nil || n != 3 {
		t.Fatalf("a page within its limit: %d, %v", n, err)
	}
}
