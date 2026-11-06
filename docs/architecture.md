# Architecture

## Request flow

```
                     ┌──────────── internal/web (chi router) ────────────┐
browser  ──/, /searches/…─▶ auth.Require  ─▶ page handler ─┐            │
                                                            │            │
script   ──/api/…──────────▶ spec guard ──▶ strict server ─┼─▶ Endpoint ─┼─▶ Store ─▶ Postgres
Claude   ──/mcp/rpc────────▶ auth.Require  ─▶ MCP tool ─────┘   (Interact│   (sqlc)
claude.ai──/oauth/…, /.well-known/…  ─▶ internal/oauth             Build) │
                     └───────────────────────────────────────────────────┘
```

Every surface ends in the same `Endpoint`. REST runs all three steps (`server.Run`). A page or an MCP tool runs `Interact` and `Build` (`server.BuildViewModel`), then renders the view model as HTML or returns it as structured tool output.

## Packages

| package | holds |
| --- | --- |
| `cmd/server` | `main`: `serve`, plus the `keys` and `invites` admin commands |
| `internal/app` | `Store` (every sqlc query plus hand-written transactions), the `Page…` lists (`Pager`) with `Paginate` and `Page` beneath them, `Links` (absolute URLs for view models), `ErrNotFound`/`ErrBadInput`, `Name` |
| `internal/server` | `Endpoint`, `Run`, `BuildViewModel`, and the `Paged` extension; `Server`, which implements the generated `StrictServerInterface`, maps errors to typed responses, and mounts the API with the spec-driven auth guard |
| `internal/server/<resource>` | one `Endpoint` per operation, each with the narrow store (and eBay) interface its `Interact` takes, plus the resource's view model and its API conversion |
| `internal/appmcp` | MCP tools and the server's `Instructions` |
| `internal/web` | the router, page handlers and templates, and the end-to-end tests |
| `internal/auth` | key and token formats, hashing, `Require`/`Optional` middleware, and the caller on the context |
| `internal/oauth` | the OAuth 2.1 authorization server (discovery, registration, authorize, token, refresh) |
| `internal/api/gen`, `internal/db/gen` | generated; never edited |

## The Endpoint pattern

```go
type Endpoint[S, Req, M, VM, Res any] interface {
	Interact(ctx context.Context, store S, req Req) (M, error) // the work, through the store it is handed
	Build(m M) VM                                              // pure: domain model → view model
	Render(vm VM) Res                                          // view model → generated response type
}
```

- `Req` is the generated `…RequestObject`, even when an MCP tool or a page is the caller. They fill one in themselves, so input validation lives in the endpoint and nowhere else.
- `S` is what the endpoint does I/O through, and it is `Interact`'s argument, never a field of the endpoint. So `Interact` is the only step that can read or write data, or call eBay: `Build` and `Render` see only the endpoint's own fields (its `Links`), and cannot reach the store even by accident.
- Each endpoint declares its `S`, a narrow interface with only the methods it calls (`searches.DeleteStore` is just `DeleteSearch`). This keeps an endpoint's reach obvious and makes a fake easy to write: a test hands `Run` a fake, with no endpoint to build around it. `*app.Store` satisfies every store interface, because it embeds `*dbgen.Queries` and sqlc generates every method. An endpoint that calls eBay takes the client in its `S` too: `listings.SearchEndpoint` takes just `listings.Searcher`, and `runs.RunEndpoint` takes `runs.RunDeps`, a store and a searcher, which its callers join with `runs.Deps(store, ebay)`.
- The caller hands the store over, converted to the endpoint's `S`: `Run(ctx, searches.ListStore(s.store), searches.ListEndpoint{Links: s.links}, req)`, and a page or an MCP tool does the same with `server.BuildViewModel`. The conversion is the injection, and the compiler checks it. (Go cannot infer `S` from the endpoint while the argument is the caller's wider store, so it is written out.)
- `Page`, below, keeps to the same rule: it is a pure method on the endpoint, like `Build`, and fetching the page happens in `Interact`, through the store.
- View models carry JSON tags, because MCP tools return them as is.
- Errors: wrap `app.ErrNotFound`, `app.ErrBadInput` or `app.ErrUpstream` with a message (`app.NotFound` converts pgx's no-rows; `app.Upstream` wraps an eBay error). `server.go` turns them into the spec's 404, 422 and 502 bodies, the pages turn them into statuses or a redirect with `?err=`, and MCP returns them as tool errors.

### Lists: `Paged`

Every list operation (searches, matches, runs) returns a page: it takes `limit` (1 to 200, 100 if left out) and `after` (the `next` of the page before), and its body carries `next`, empty on the last page. Pages are keyset, not offset: a list's query orders by a key that is unique per row (its sort columns, ending in the id) and starts after the key of the last row the caller saw, so a page is as cheap at the end of a list as at the start, and a row added or deleted while someone pages neither repeats nor skips the rows around it. That matters for matches: a run adds many at once, all first seen in the same transaction, and the id breaks the tie between them.

A list endpoint implements one more method beside the three, naming its list and reading the page off the request. Its model is that one page, which `Interact` asks its store for like any other read:

```go
type Paged[Req any] interface {
	Page(req Req) app.PageRequest // the list's name, and the limit and after the caller sent
}

type ListStore interface {
	PageRuns(ctx context.Context, searchID string, p app.PageRequest) (app.Page[dbgen.SearchRun], error)
}

func (ListEndpoint) Page(req apigen.ListRunsRequestObject) app.PageRequest {
	return app.PageRequest{List: "run", Limit: req.Params.Limit, After: req.Params.After}
}

func (e ListEndpoint) Interact(ctx context.Context, store ListStore, req apigen.ListRunsRequestObject) (app.Page[dbgen.SearchRun], error) {
	return store.PageRuns(ctx, req.Params.SearchId, e.Page(req))
}
```

The paging is the store's business, in `internal/app/pages.go`: each `Page…` method (`app.Pager` lists them) runs its keyset query (`RunsAfter`) with its sort key through `app.Paginate`, so an endpoint knows nothing of keys or cursors, and a fake store returns a page outright. `app.Paginate` checks the limit, decodes the cursor (refusing one from another list, or one that is not a cursor, with `ErrBadInput`), asks the query for one row more than the page so it knows whether there is a next page without counting, and encodes the last row's key as `next`. `Build` and `Render` map the page with `app.MapPage`. The cursor is the key in base64 JSON with the list's name: opaque to callers, and a position, not a permission.

Every surface pages: the API by `limit` and `after`, the `list_` MCP tools by the same arguments (the server's instructions tell Claude to follow `next`), and the pages by Older links that carry `after` (the index's searches, a search page's matches within its tab). Code that needs a whole list follows `next` with `server.All`, or keeps a `List…` query of every row beside the `…After` one.

Three things keep a list from going unpaged:

- `server.go` runs every list through `RunPaged`, which takes a `PagedEndpoint` (a `Paged` endpoint whose model is an `app.Page`), so a list endpoint that returns a slice does not compile.
- `Run` fails closed if a paged endpoint returned more than the caller asked for (`TestOverlongPageFailsClosed`).
- `TestEveryListIsPaged` checks the spec: every `list…` operation takes `limit` and `after`, its body requires `next`, and it declares the 422.

## Conventions

- **xid ids** in `CHAR(20)`. They are sortable, URL-safe and short enough to read aloud.
- **Soft delete everywhere.** Every live table has `created_at`, `updated_at`, `deleted_at`, and every query filters `deleted_at IS NULL`. A delete is an `UPDATE`, and `:execrows` tells the endpoint whether anything was there.
- **Partial updates.** A pointer field in the spec (`x-go-type-skip-optional-pointer: false`) means a PATCH sends only what changes. When no field can be NULL, `COALESCE(sqlc.narg(x), x)` in SQL applies it. Searches cannot use that, because a price limit of NULL means "no limit": `searches.UpdateEndpoint` lays the patch over the stored row and writes every field. Everything else optional stays a plain value (`prefer-skip-optional-pointer`), so conversions need no pointer per field.
- **No tenants.** Every key sees the same searches, and the api key behind a request is recorded on what it creates (`created_by`). See "Adding tenants" below if your app needs isolation.

## Why it is this way

Most of these were learned the hard way in inventory or Creature Lab.

- **The API is generated from OpenAPI.** Creature Lab first shipped with hand-written JSON routes on the theory that it was too small to need a generator. That left no contract: a client had to read the Go to learn a body's shape, and public versus private lived in router groups nobody could audit at a glance. With the spec, the strict server will only return responses the spec declares, and `var _ apigen.StrictServerInterface = (*Server)(nil)` fails the build until every operation is implemented.
- **Auth is decided from the spec, not by oapi-codegen.** oapi-codegen v2.8's chi server does not put an operation's security requirements on the request context. Its strict middleware knows the operation, but it runs after the body is decoded, so an anonymous caller sending a bad body would get a 400 instead of a 401. So `Server.Mount` installs a chi middleware that looks up the matched route pattern in `PublicOperations()` (read from the embedded spec) and fails closed for anything not listed.
- **Server-rendered pages, no React.** Inventory ships a Vite/React UI, which means an npm build on every deploy and a second toolchain to keep current. Creature Lab's html/template pages mean the binary is the whole deploy. They also allow a `default-src 'none'` CSP, which matters when the pages show text a model wrote. If you need a rich client, add one, but consider whether forms that post and redirect will do first.
- **Generators run through `go run …@version`.** sqlc, oapi-codegen and dbmate stay out of `go.mod`, so their large dependency trees do not land in the app's module graph, and the versions are pinned in one place (the Makefile).
- **MCP is at `/mcp/rpc`, not `/mcp`.** The sprites.dev proxy intercepts the bare `/mcp` path, and POSTs hang before reaching the app.
- **The server never calls a model.** The connected client (the person's own Claude) does the thinking, so usage costs whatever their plan costs. If you add server-side generation, put it behind its own key, a stored budget, and a cost recorded per call.
- **An empty `{{define}}` does not override a `{{block}}`.** Go templates treat a whitespace-only definition as empty and keep the original. Creature Lab's login page did this and ended up with a Sign out button. Give an override some content.

## Adding tenants

Inventory is multi-tenant; this app, like the template it came from, is not. To add tenants:

1. Add a `tenants` table and a `tenant_id` column on `api_keys` and on every resource table. Scope the unique indexes by tenant.
2. Have `auth.Resolve` return the key's tenant too, and put it on the context (`auth.TenantID(ctx)`).
3. Give every query a `tenant_id = $n` filter, and every endpoint `auth.TenantID(ctx)` to pass in. Never take a tenant as a request field or a tool argument.
4. Mint tenants from the CLI (`server bootstrap --tenant NAME`), never over the API.
