package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	dbgen "github.com/KevinMcHugh/mr-ebay/internal/db/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/me"
	"github.com/KevinMcHugh/mr-ebay/internal/server/runs"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// BaseURL is where the API is mounted: the spec's servers entry.
const BaseURL = "/api"

// Store is everything the endpoints persist through. *app.Store satisfies
// it; each endpoint narrows it to its own interface. Add a method here when
// an endpoint needs a transaction that app.Store implements by hand.
type Store interface {
	dbgen.Querier
	RecordResults(ctx context.Context, searchID string, items []ebay.Item) ([]app.Recorded, error)
}

// Server implements apigen.StrictServerInterface by composing per-resource
// endpoints.
type Server struct {
	store Store
	ebay  ebay.Searcher
	links app.Links
}

// New builds the API on store and the eBay client, with links for the URLs
// in its responses.
func New(store Store, eb ebay.Searcher, links app.Links) *Server {
	return &Server{store: store, ebay: eb, links: links}
}

// Compile-time assertion: this fails to build until every operation in the
// spec has a method below.
var _ apigen.StrictServerInterface = (*Server)(nil)

// -----------------------------------------------------------------------------
// Mounting
// -----------------------------------------------------------------------------

// Mount attaches the API to r under BaseURL. requireAuth guards every
// operation except those the spec marks public with `security: []`, so the
// spec, not the router, decides who may call what.
func (s *Server) Mount(r chi.Router, requireAuth func(http.Handler) http.Handler) {
	public := PublicOperations()
	guard := func(next http.Handler) http.Handler {
		protected := requireAuth(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			pattern := strings.TrimPrefix(chi.RouteContext(r.Context()).RoutePattern(), BaseURL)
			if public[r.Method+" "+pattern] {
				// Anyone may read these, so any site may too. No
				// credentials are allowed cross-site, so this opens nothing
				// an anonymous caller could not already get.
				w.Header().Set("Access-Control-Allow-Origin", "*")
				next.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
	strict := apigen.NewStrictHandlerWithOptions(s, nil, apigen.StrictHTTPServerOptions{
		RequestErrorHandlerFunc:  badRequest,
		ResponseErrorHandlerFunc: internalError,
	})
	r.Group(func(r chi.Router) {
		r.Use(limitBody)
		apigen.HandlerWithOptions(strict, apigen.ChiServerOptions{
			BaseURL:          BaseURL,
			BaseRouter:       r,
			Middlewares:      []apigen.MiddlewareFunc{guard},
			ErrorHandlerFunc: badRequest,
		})
	})
}

// PublicOperations is the set of operations ("GET /searches/{searchId}") whose
// spec entry overrides the global security with an empty list. An operation
// that does not mention security inherits the global requirement, so a new
// endpoint is private until the spec says otherwise.
func PublicOperations() map[string]bool {
	spec, err := apigen.GetSwagger()
	if err != nil {
		// The spec is embedded at build time, so this is a broken build.
		panic("embedded OpenAPI spec: " + err.Error())
	}
	public := map[string]bool{}
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Security != nil && len(*op.Security) == 0 {
				public[method+" "+path] = true
			}
		}
	}
	return public
}

func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, app.MaxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

func badRequest(w http.ResponseWriter, _ *http.Request, err error) {
	writeError(w, http.StatusBadRequest, err.Error())
}

func internalError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("api", "path", r.URL.Path, "err", err)
	writeError(w, http.StatusInternalServerError, "internal error")
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(apigen.Error{Error: msg})
}

// -----------------------------------------------------------------------------
// Errors
// -----------------------------------------------------------------------------

func isNotFound(err error) bool { return errors.Is(err, app.ErrNotFound) }

// isUnprocessable is a request that parsed but cannot be done: a field out
// of bounds.
func isUnprocessable(err error) bool { return errors.Is(err, app.ErrBadInput) }

// isUpstream is eBay failing, refusing, or not being configured.
func isUpstream(err error) bool { return errors.Is(err, app.ErrUpstream) }

func upstream(err error) apigen.UpstreamJSONResponse {
	return apigen.UpstreamJSONResponse{Error: err.Error()}
}

func notFound(err error) apigen.NotFoundJSONResponse {
	return apigen.NotFoundJSONResponse{Error: err.Error()}
}

func unprocessable(err error) apigen.UnprocessableJSONResponse {
	return apigen.UnprocessableJSONResponse{Error: err.Error()}
}

func bodyRequired() apigen.BadRequestJSONResponse {
	return apigen.BadRequestJSONResponse{Error: "body required"}
}

// -----------------------------------------------------------------------------
// Me
// -----------------------------------------------------------------------------

func (s *Server) GetMe(ctx context.Context, req apigen.GetMeRequestObject) (apigen.GetMeResponseObject, error) {
	return Run(ctx, me.Endpoint{Store: s.store}, req)
}

// -----------------------------------------------------------------------------
// Searches
// -----------------------------------------------------------------------------

func (s *Server) ListSearches(ctx context.Context, req apigen.ListSearchesRequestObject) (apigen.ListSearchesResponseObject, error) {
	return Run(ctx, searches.ListEndpoint{Store: s.store, Links: s.links}, req)
}

func (s *Server) GetSearch(ctx context.Context, req apigen.GetSearchRequestObject) (apigen.GetSearchResponseObject, error) {
	resp, err := Run(ctx, searches.GetEndpoint{Store: s.store, Links: s.links}, req)
	if isNotFound(err) {
		return apigen.GetSearch404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	}
	return resp, err
}

func (s *Server) CreateSearch(ctx context.Context, req apigen.CreateSearchRequestObject) (apigen.CreateSearchResponseObject, error) {
	if req.Body == nil {
		return apigen.CreateSearch400JSONResponse{BadRequestJSONResponse: bodyRequired()}, nil
	}
	resp, err := Run(ctx, searches.CreateEndpoint{Store: s.store, Links: s.links}, req)
	if isUnprocessable(err) {
		return apigen.CreateSearch422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	}
	return resp, err
}

func (s *Server) UpdateSearch(ctx context.Context, req apigen.UpdateSearchRequestObject) (apigen.UpdateSearchResponseObject, error) {
	if req.Body == nil {
		return apigen.UpdateSearch400JSONResponse{BadRequestJSONResponse: bodyRequired()}, nil
	}
	resp, err := Run(ctx, searches.UpdateEndpoint{Store: s.store, Links: s.links}, req)
	switch {
	case isNotFound(err):
		return apigen.UpdateSearch404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	case isUnprocessable(err):
		return apigen.UpdateSearch422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	}
	return resp, err
}

func (s *Server) DeleteSearch(ctx context.Context, req apigen.DeleteSearchRequestObject) (apigen.DeleteSearchResponseObject, error) {
	resp, err := Run(ctx, searches.DeleteEndpoint{Store: s.store}, req)
	if isNotFound(err) {
		return apigen.DeleteSearch404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	}
	return resp, err
}

// -----------------------------------------------------------------------------
// eBay: ad-hoc search and runs
// -----------------------------------------------------------------------------

func (s *Server) SearchEbay(ctx context.Context, req apigen.SearchEbayRequestObject) (apigen.SearchEbayResponseObject, error) {
	resp, err := Run(ctx, listings.SearchEndpoint{Ebay: s.ebay}, req)
	switch {
	case isUnprocessable(err):
		return apigen.SearchEbay422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	case isUpstream(err):
		return apigen.SearchEbay502JSONResponse{UpstreamJSONResponse: upstream(err)}, nil
	}
	return resp, err
}

func (s *Server) RunSearch(ctx context.Context, req apigen.RunSearchRequestObject) (apigen.RunSearchResponseObject, error) {
	resp, err := Run(ctx, runs.RunEndpoint{Store: s.store, Ebay: s.ebay, Links: s.links}, req)
	switch {
	case isNotFound(err):
		return apigen.RunSearch404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	case isUpstream(err):
		return apigen.RunSearch502JSONResponse{UpstreamJSONResponse: upstream(err)}, nil
	}
	return resp, err
}

func (s *Server) ListRuns(ctx context.Context, req apigen.ListRunsRequestObject) (apigen.ListRunsResponseObject, error) {
	resp, err := Run(ctx, runs.ListEndpoint{Store: s.store}, req)
	if isUnprocessable(err) {
		return apigen.ListRuns422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	}
	return resp, err
}

// -----------------------------------------------------------------------------
// Matches and listings
// -----------------------------------------------------------------------------

func (s *Server) ListMatches(ctx context.Context, req apigen.ListMatchesRequestObject) (apigen.ListMatchesResponseObject, error) {
	resp, err := Run(ctx, matches.ListEndpoint{Store: s.store, Links: s.links}, req)
	switch {
	case isNotFound(err):
		return apigen.ListMatches404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	case isUnprocessable(err):
		return apigen.ListMatches422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	}
	return resp, err
}

func (s *Server) SetMatchStatus(ctx context.Context, req apigen.SetMatchStatusRequestObject) (apigen.SetMatchStatusResponseObject, error) {
	if req.Body == nil {
		return apigen.SetMatchStatus400JSONResponse{BadRequestJSONResponse: bodyRequired()}, nil
	}
	resp, err := Run(ctx, matches.SetStatusEndpoint{Store: s.store}, req)
	if isUnprocessable(err) {
		return apigen.SetMatchStatus422JSONResponse{UnprocessableJSONResponse: unprocessable(err)}, nil
	}
	return resp, err
}

func (s *Server) GetListing(ctx context.Context, req apigen.GetListingRequestObject) (apigen.GetListingResponseObject, error) {
	resp, err := Run(ctx, listings.GetEndpoint{Store: s.store, Links: s.links}, req)
	if isNotFound(err) {
		return apigen.GetListing404JSONResponse{NotFoundJSONResponse: notFound(err)}, nil
	}
	return resp, err
}
