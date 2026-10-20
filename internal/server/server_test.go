package server_test

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
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

// TestAnonymousCallers checks every operation refuses a caller with no
// credential before it reaches the store: the nil Querier panics if one does.
func TestAnonymousCallers(t *testing.T) {
	r := chi.NewRouter()
	deny := func(http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	server.New(struct{ dbgen.Querier }{}, app.NewLinks("https://app.example")).Mount(r, deny)

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/me"},
		{"GET", "/api/searches"},
		{"POST", "/api/searches"},
		{"GET", "/api/searches/s1"},
		{"PATCH", "/api/searches/s1"},
		{"DELETE", "/api/searches/s1"},
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
