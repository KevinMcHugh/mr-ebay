// Package appmcp exposes the app over the Model Context Protocol. Each tool
// drives the same Endpoint the matching REST operation runs
// (server.BuildViewModel: Interact + Build) and wraps the view model in its
// own output in place of Render, so a rule lives in one place whichever
// surface calls it.
//
// The caller is already authenticated by the time a tool runs (the /mcp/rpc
// route requires a credential), so a tool reads who it is acting as from
// the context with auth.KeyID, and never takes it as an argument.
package appmcp

import (
	"context"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	apigen "github.com/KevinMcHugh/mr-ebay/internal/api/gen"
	"github.com/KevinMcHugh/mr-ebay/internal/app"
	"github.com/KevinMcHugh/mr-ebay/internal/ebay"
	"github.com/KevinMcHugh/mr-ebay/internal/server"
	"github.com/KevinMcHugh/mr-ebay/internal/server/listings"
	"github.com/KevinMcHugh/mr-ebay/internal/server/matches"
	"github.com/KevinMcHugh/mr-ebay/internal/server/me"
	"github.com/KevinMcHugh/mr-ebay/internal/server/runs"
	"github.com/KevinMcHugh/mr-ebay/internal/server/searches"
)

// Instructions is sent to every client on connect. Tell the model what the
// app is for and the usual order of tools; keep it short.
const Instructions = `Mr. eBay keeps the user's saved eBay searches: what they are shopping for, as eBay filters plus a free-text "wants" describing what a filter cannot (fit, flaws to avoid, sellers to trust).

Finding things:
1. search_ebay tries a query without saving anything. Use it to tune keywords and filters with the user.
2. create_search saves it. Put hard filters in the filter fields and everything else the user said in wants.
3. run_search fetches from eBay and returns the matches this search had not found before, plus price drops. Judge each one against the search's wants and tell the user about the ones that fit, with their links. Mark what you reported seen with set_match_status, and dismissed what clearly does not fit, so the next run only surfaces what is new.
list_matches shows what a search has found (filter by status: new, seen, starred, dismissed), and get_listing gives one listing's price history and every search that found it. list_runs says whether runs are working, with their errors.

list_searches and get_search show what the user is after; update_search and delete_search change them (confirm before deleting). Prices are integer cents of the search's currency (USD unless set), and 0 means no limit. Set watch only when the user wants to hear about new matches without asking.`

// NewServer builds the MCP server with every tool registered.
func NewServer(store server.Store, eb ebay.Searcher, links app.Links) *mcpsdk.Server {
	s := mcpsdk.NewServer(&mcpsdk.Implementation{Name: app.Name, Version: "0.1.0"},
		&mcpsdk.ServerOptions{Instructions: Instructions})
	registerRead(s, store, links)
	registerWrite(s, store, links)
	registerEbay(s, store, eb, links)
	return s
}

// ---- inputs and outputs -----------------------------------------------------

type Empty struct{}

type MeOutput struct {
	Me me.Me `json:"me"`
}

type ListSearchesOutput struct {
	Searches []searches.Search `json:"searches"`
}

type SearchIDInput struct {
	ID string `json:"id" jsonschema:"search id"`
}

type SearchOutput struct {
	Search searches.Search `json:"search"`
}

type CreateSearchInput struct {
	Name            string   `json:"name" jsonschema:"a short name for what is wanted, e.g. rain shell size M"`
	Query           string   `json:"query" jsonschema:"eBay keywords"`
	CategoryIDs     []string `json:"categoryIds,omitempty" jsonschema:"eBay category ids to search within"`
	Conditions      []string `json:"conditions,omitempty" jsonschema:"any of NEW, USED, UNSPECIFIED; omit for any condition"`
	BuyingOptions   []string `json:"buyingOptions,omitempty" jsonschema:"any of FIXED_PRICE, AUCTION, BEST_OFFER; omit for any"`
	MinPriceCents   int64    `json:"minPriceCents,omitempty" jsonschema:"lowest price in cents; 0 or omitted for none"`
	MaxPriceCents   int64    `json:"maxPriceCents,omitempty" jsonschema:"highest price in cents; 0 or omitted for none"`
	Currency        string   `json:"currency,omitempty" jsonschema:"three-letter currency code, default USD"`
	Marketplace     string   `json:"marketplace,omitempty" jsonschema:"eBay marketplace id, default EBAY_US"`
	Wants           string   `json:"wants,omitempty" jsonschema:"what the user is after in their own words: size, fit, must-haves, deal-breakers"`
	Watch           bool     `json:"watch,omitempty" jsonschema:"check it unattended and alert on new matches"`
	AlertBelowCents int64    `json:"alertBelowCents,omitempty" jsonschema:"for a watched search, alert only on matches at or below this total in cents; 0 alerts on every new match"`
}

type UpdateSearchInput struct {
	ID              string    `json:"id" jsonschema:"search id"`
	Name            *string   `json:"name,omitempty" jsonschema:"new name; omit to keep"`
	Query           *string   `json:"query,omitempty" jsonschema:"new keywords; omit to keep"`
	CategoryIDs     *[]string `json:"categoryIds,omitempty" jsonschema:"replacement category ids; omit to keep"`
	Conditions      *[]string `json:"conditions,omitempty" jsonschema:"replacement conditions; [] for any; omit to keep"`
	BuyingOptions   *[]string `json:"buyingOptions,omitempty" jsonschema:"replacement buying options; [] for any; omit to keep"`
	MinPriceCents   *int64    `json:"minPriceCents,omitempty" jsonschema:"new minimum in cents; 0 removes it; omit to keep"`
	MaxPriceCents   *int64    `json:"maxPriceCents,omitempty" jsonschema:"new maximum in cents; 0 removes it; omit to keep"`
	Currency        *string   `json:"currency,omitempty" jsonschema:"omit to keep"`
	Marketplace     *string   `json:"marketplace,omitempty" jsonschema:"omit to keep"`
	Wants           *string   `json:"wants,omitempty" jsonschema:"replacement wants; omit to keep"`
	Watch           *bool     `json:"watch,omitempty" jsonschema:"turn unattended watching on or off; omit to keep"`
	AlertBelowCents *int64    `json:"alertBelowCents,omitempty" jsonschema:"new alert threshold in cents; 0 alerts on every new match; omit to keep"`
}

type OKOutput struct {
	OK bool `json:"ok"`
}

// ---- registration -----------------------------------------------------------

func registerRead(s *mcpsdk.Server, store server.Store, links app.Links) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "whoami",
		Description: "The api key this connection acts as.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ Empty) (*mcpsdk.CallToolResult, MeOutput, error) {
		vm, err := server.BuildViewModel(ctx, me.Endpoint{Store: store}, apigen.GetMeRequestObject{})
		return nil, MeOutput{Me: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "list_searches",
		Description: "List the user's saved eBay searches, newest first, with how many matches they have not looked at yet.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, _ Empty) (*mcpsdk.CallToolResult, ListSearchesOutput, error) {
		vm, err := server.BuildViewModel(ctx, searches.ListEndpoint{Store: store, Links: links}, apigen.ListSearchesRequestObject{})
		return nil, ListSearchesOutput{Searches: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "get_search",
		Description: "One saved search in full, including the user's wants in their own words.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in SearchIDInput) (*mcpsdk.CallToolResult, SearchOutput, error) {
		vm, err := server.BuildViewModel(ctx, searches.GetEndpoint{Store: store, Links: links}, apigen.GetSearchRequestObject{SearchId: in.ID})
		return nil, SearchOutput{Search: vm}, err
	})
}

func registerWrite(s *mcpsdk.Server, store server.Store, links app.Links) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "create_search",
		Description: "Save an eBay search. Put the hard filters in the filter fields and everything else the user said about what they want in wants. Saving does not run it.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in CreateSearchInput) (*mcpsdk.CallToolResult, SearchOutput, error) {
		vm, err := server.BuildViewModel(ctx, searches.CreateEndpoint{Store: store, Links: links}, apigen.CreateSearchRequestObject{
			Body: &apigen.SearchCreate{
				Name: in.Name, Query: in.Query, CategoryIds: in.CategoryIDs, Conditions: in.Conditions,
				BuyingOptions: in.BuyingOptions, MinPriceCents: in.MinPriceCents, MaxPriceCents: in.MaxPriceCents,
				Currency: in.Currency, Marketplace: in.Marketplace, Wants: in.Wants, Watch: in.Watch,
				AlertBelowCents: in.AlertBelowCents,
			},
		})
		return nil, SearchOutput{Search: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "update_search",
		Description: "Change a saved search. Fields left out stay as they are.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in UpdateSearchInput) (*mcpsdk.CallToolResult, SearchOutput, error) {
		vm, err := server.BuildViewModel(ctx, searches.UpdateEndpoint{Store: store, Links: links}, apigen.UpdateSearchRequestObject{
			SearchId: in.ID,
			Body: &apigen.SearchUpdate{
				Name: in.Name, Query: in.Query, CategoryIds: in.CategoryIDs, Conditions: in.Conditions,
				BuyingOptions: in.BuyingOptions, MinPriceCents: in.MinPriceCents, MaxPriceCents: in.MaxPriceCents,
				Currency: in.Currency, Marketplace: in.Marketplace, Wants: in.Wants, Watch: in.Watch,
				AlertBelowCents: in.AlertBelowCents,
			},
		})
		return nil, SearchOutput{Search: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "delete_search",
		Description: "Delete a saved search. Confirm with the user first.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in SearchIDInput) (*mcpsdk.CallToolResult, OKOutput, error) {
		ok, err := server.BuildViewModel(ctx, searches.DeleteEndpoint{Store: store}, apigen.DeleteSearchRequestObject{SearchId: in.ID})
		return nil, OKOutput{OK: ok}, err
	})
}

// ---- eBay, matches and listings ---------------------------------------------

type SearchEbayInput struct {
	Query         string   `json:"query" jsonschema:"eBay keywords"`
	CategoryIDs   []string `json:"categoryIds,omitempty" jsonschema:"eBay category ids to search within"`
	Conditions    []string `json:"conditions,omitempty" jsonschema:"any of NEW, USED, UNSPECIFIED; omit for any"`
	BuyingOptions []string `json:"buyingOptions,omitempty" jsonschema:"any of FIXED_PRICE, AUCTION, BEST_OFFER; omit for any"`
	MinPriceCents int64    `json:"minPriceCents,omitempty" jsonschema:"lowest price in cents; omit for none"`
	MaxPriceCents int64    `json:"maxPriceCents,omitempty" jsonschema:"highest price in cents; omit for none"`
	Currency      string   `json:"currency,omitempty" jsonschema:"three-letter currency code, default USD"`
	Marketplace   string   `json:"marketplace,omitempty" jsonschema:"eBay marketplace id, default EBAY_US"`
	MaxItems      int      `json:"maxItems,omitempty" jsonschema:"how many listings to return, 1-200 (default 50)"`
}

type SearchEbayOutput struct {
	Total    int                `json:"total"`
	Listings []listings.Listing `json:"listings"`
}

type RunOutput struct {
	runs.Result
}

type ListMatchesInput struct {
	SearchID string `json:"searchId" jsonschema:"search id"`
	Status   string `json:"status,omitempty" jsonschema:"only this status: new, seen, starred or dismissed; omit for all"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many, 1-500 (default 100)"`
}

type ListMatchesOutput struct {
	Matches []matches.Match `json:"matches"`
}

type SetMatchStatusInput struct {
	IDs    []string `json:"ids" jsonschema:"match ids (not listing ids)"`
	Status string   `json:"status" jsonschema:"seen, starred, dismissed, or new to unsee"`
}

type SetMatchStatusOutput struct {
	Updated int64 `json:"updated"`
}

type ListingIDInput struct {
	ID string `json:"id" jsonschema:"listing id (from a match's listing.id)"`
}

type ListingOutput struct {
	Listing listings.Detail `json:"listing"`
}

type ListRunsInput struct {
	SearchID string `json:"searchId,omitempty" jsonschema:"only this search's runs; omit for all"`
	Limit    int    `json:"limit,omitempty" jsonschema:"at most this many, 1-200 (default 20)"`
}

type ListRunsOutput struct {
	Runs []runs.Run `json:"runs"`
}

func registerEbay(s *mcpsdk.Server, store server.Store, eb ebay.Searcher, links app.Links) {
	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "search_ebay",
		Description: "Search eBay now without saving anything. Use it to try keywords and filters before create_search. Each call uses eBay quota, so do not loop on it.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in SearchEbayInput) (*mcpsdk.CallToolResult, SearchEbayOutput, error) {
		vm, err := server.BuildViewModel(ctx, listings.SearchEndpoint{Ebay: eb}, apigen.SearchEbayRequestObject{
			Params: apigen.SearchEbayParams{
				Q: in.Query, CategoryIds: in.CategoryIDs, Conditions: in.Conditions, BuyingOptions: in.BuyingOptions,
				MinPriceCents: in.MinPriceCents, MaxPriceCents: in.MaxPriceCents, Currency: in.Currency,
				Marketplace: in.Marketplace, MaxItems: in.MaxItems,
			},
		})
		return nil, SearchEbayOutput{Total: vm.Total, Listings: vm.Listings}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "run_search",
		Description: "Run a saved search against eBay now and store what it finds. Returns the matches this search had not found before (newMatches) and listings whose total dropped (priceDrops). Matches it found before and that did not drop are stored but not returned; see list_matches.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in SearchIDInput) (*mcpsdk.CallToolResult, RunOutput, error) {
		vm, err := server.BuildViewModel(ctx, runs.RunEndpoint{Store: store, Ebay: eb, Links: links}, apigen.RunSearchRequestObject{SearchId: in.ID})
		return nil, RunOutput{Result: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "list_matches",
		Description: "What a saved search has found, newest first, each with its listing. Filter by status to see only new, seen, starred or dismissed ones.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ListMatchesInput) (*mcpsdk.CallToolResult, ListMatchesOutput, error) {
		vm, err := server.BuildViewModel(ctx, matches.ListEndpoint{Store: store, Links: links}, apigen.ListMatchesRequestObject{
			SearchId: in.SearchID, Params: apigen.ListMatchesParams{Status: apigen.MatchStatus(in.Status), Limit: in.Limit},
		})
		return nil, ListMatchesOutput{Matches: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "set_match_status",
		Description: "Mark matches seen (reported to the user), starred (the user likes it), dismissed (does not fit), or new again. Status is per search: dismissing here does not hide the listing from another search.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in SetMatchStatusInput) (*mcpsdk.CallToolResult, SetMatchStatusOutput, error) {
		n, err := server.BuildViewModel(ctx, matches.SetStatusEndpoint{Store: store}, apigen.SetMatchStatusRequestObject{
			Body: &apigen.SetMatchStatusJSONRequestBody{Ids: in.IDs, Status: apigen.MatchStatus(in.Status)},
		})
		return nil, SetMatchStatusOutput{Updated: n}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "get_listing",
		Description: "One stored listing with its price history (asking prices over time, newest first) and every search that found it.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ListingIDInput) (*mcpsdk.CallToolResult, ListingOutput, error) {
		vm, err := server.BuildViewModel(ctx, listings.GetEndpoint{Store: store, Links: links}, apigen.GetListingRequestObject{ListingId: in.ID})
		return nil, ListingOutput{Listing: vm}, err
	})

	mcpsdk.AddTool(s, &mcpsdk.Tool{
		Name:        "list_runs",
		Description: "Recent runs, newest first, with how many results each found and any error. Use it to check that searches, and the watcher, are working.",
	}, func(ctx context.Context, _ *mcpsdk.CallToolRequest, in ListRunsInput) (*mcpsdk.CallToolResult, ListRunsOutput, error) {
		vm, err := server.BuildViewModel(ctx, runs.ListEndpoint{Store: store}, apigen.ListRunsRequestObject{
			Params: apigen.ListRunsParams{SearchId: in.SearchID, Limit: in.Limit},
		})
		return nil, ListRunsOutput{Runs: vm}, err
	})
}
