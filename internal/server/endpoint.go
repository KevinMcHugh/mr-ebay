// Package server is the REST layer. api/openapi.yaml is the source of
// truth, oapi-codegen turns it into a strict server interface
// (internal/api/gen), and Server implements that interface by running one
// Endpoint per operation. The MCP tools and the web pages drive the same
// endpoints through BuildViewModel, so a rule lives in one place whichever
// surface calls it. See docs/architecture.md.
package server

import (
	"context"
	"fmt"

	"github.com/KevinMcHugh/mr-ebay/internal/app"
)

// Endpoint is the MVVM contract for a single HTTP operation.
//
//   - S is the store the endpoint reads and writes through: a narrow
//     interface the endpoint declares itself, listing only the methods it
//     calls. *app.Store satisfies every one.
//   - Req is the parsed input (an oapi-codegen *RequestObject).
//   - M  is the domain model an Interact produces from the store.
//   - VM is the view model, a pure transformation of M for presentation.
//   - Res is the typed response (an oapi-codegen *ResponseObject).
//
// The store is Interact's argument, never a field of the endpoint, so
// Interact is the only step that can touch data: Build and Render, which
// see only the endpoint's own fields (its Links, say), cannot. The caller
// hands the store over, converting its own to the endpoint's S, which
// states, and the compiler checks, exactly what the endpoint may use:
//
//	Run(ctx, searches.ListStore(s.store), searches.ListEndpoint{Links: s.links}, req)
//
// An endpoint that also calls eBay takes both in its S (runs.RunDeps), and
// its caller joins them (runs.Deps(s.store, s.ebay)): eBay is I/O too, so it
// is Interact's to use, not a field.
type Endpoint[S, Req, M, VM, Res any] interface {
	Interact(ctx context.Context, store S, req Req) (M, error)
	Build(m M) VM
	Render(vm VM) Res
}

// Interactor is the Interact+Build half of Endpoint. Every Endpoint
// satisfies it, so the MCP tools and web pages reuse an endpoint's logic and
// view model while presenting it their own way.
type Interactor[S, Req, M, VM any] interface {
	Interact(ctx context.Context, store S, req Req) (M, error)
	Build(m M) VM
}

// Run drives an Endpoint end to end with store, enforcing the length of
// its page if it is Paged.
func Run[S, Req, M, VM, Res any](ctx context.Context, store S, e Endpoint[S, Req, M, VM, Res], req Req) (Res, error) {
	vm, err := BuildViewModel[S, Req, M, VM](ctx, store, e, req)
	if err != nil {
		var zero Res
		return zero, err
	}
	return e.Render(vm), nil
}

// BuildViewModel drives Interact with store, then Build, stopping short of
// Render. It enforces a page's length exactly as Run does.
func BuildViewModel[S, Req, M, VM any](ctx context.Context, store S, e Interactor[S, Req, M, VM], req Req) (VM, error) {
	m, err := e.Interact(ctx, store, req)
	if err == nil {
		err = checkPage(e, req, m)
	}
	if err != nil {
		var zero VM
		return zero, err
	}
	return e.Build(m), nil
}

// Paged is the extension a list endpoint implements: Page names the list
// and reads the page the caller asked for (the limit and after query
// parameters every list operation takes). It is pure, like Build. The
// endpoint's model is that one page, app.Page[Row], which Interact asks its
// store for (a Page… method of app.Pager, in the endpoint's S):
//
//	func (ListEndpoint) Page(req apigen.ListSearchesRequestObject) app.PageRequest {
//		return app.PageRequest{List: "search", Limit: req.Params.Limit, After: req.Params.After}
//	}
//
// Run and BuildViewModel check what comes back: a page longer than the
// caller asked for fails closed, since it means the endpoint ignored the
// limit and a large list would come back whole.
type Paged[Req any] interface {
	Page(req Req) app.PageRequest
}

// PagedEndpoint is a list endpoint: a Paged Endpoint whose model is one
// page of rows and whose view model is one page of views. Server's
// handlers for the list operations run theirs through RunPaged, so a list
// endpoint that does not page fails to compile.
type PagedEndpoint[S, Req, Row, VM, Res any] interface {
	Endpoint[S, Req, app.Page[Row], app.Page[VM], Res]
	Paged[Req]
}

// RunPaged is Run for a list endpoint.
func RunPaged[S, Req, Row, VM, Res any](ctx context.Context, store S, e PagedEndpoint[S, Req, Row, VM, Res], req Req) (Res, error) {
	return Run[S, Req, app.Page[Row], app.Page[VM], Res](ctx, store, e, req)
}

// checkPage refuses a page longer than e's request allows.
func checkPage[Req any](e any, req Req, m any) error {
	p, ok := e.(Paged[Req])
	if !ok {
		return nil
	}
	l, ok := m.(interface{ Len() int })
	if !ok {
		return fmt.Errorf("server: %T is Paged but its model %T is not a page", e, m)
	}
	size, err := p.Page(req).Size()
	if err != nil {
		return err
	}
	if l.Len() > size {
		return fmt.Errorf("server: %T returned %d items for a page of %d", e, l.Len(), size)
	}
	return nil
}

// All is every item of a paged list, following next from page to page,
// for a surface that needs the whole list at once (an MCP tool that
// gathers several lists, say). req makes the request for the page that
// starts at after. Prefer handing the caller a page and its next.
func All[S, Req, Row, VM any](ctx context.Context, store S, e Interactor[S, Req, app.Page[Row], app.Page[VM]], req func(after string) Req) ([]VM, error) {
	var out []VM
	after := ""
	for {
		page, err := BuildViewModel(ctx, store, e, req(after))
		if err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.Next == "" {
			return out, nil
		}
		after = page.Next
	}
}
