# Mr. eBay

Your eBay shopping, kept by a small server that Claude talks to over the [Model Context Protocol](https://modelcontextprotocol.io). Save what you are looking for as a **search** (eBay filters, plus what you want in your own words). Mr. eBay finds and tracks matching listings, remembers what you have already seen, and records how prices move. Claude handles the judgment calls. Watched searches run unattended and raise alerts.

One Go binary serves the pages, a JSON API and MCP, and deploys to a [sprites.dev](https://sprites.dev) sprite. It was created from [app-template](https://github.com/KevinMcHugh/app-template) (at 17e2fbf), so its architecture, auth and deploy are the template's. [docs/architecture.md](docs/architecture.md) explains them.

**Status:** saved searches, ad-hoc eBay searches, and running a saved search (listings, matches, price history, the run log) all work over REST, MCP and the pages, tested against Postgres and a fake eBay. Unattended watching is designed but not built. The eBay client has not yet made a call to the real eBay. [docs/design.md](docs/design.md) has the schema, the plan and the build order.

## Why it is its own app

It could have been integrations bolted onto [inventory](https://github.com/KevinMcHugh/inventory). It is not, because inventory's value is being a generic datastore with no behavior. Mr. eBay is mostly behavior: an eBay client, polling, de-duplication and price tracking. Keeping them apart keeps inventory generic, gives this app tables shaped for its job, contains failures, and gives Claude a small, focused set of tools. Claude connects the two in conversation.

## Run it locally

Prereqs: Go (the version in `go.mod`; `go` fetches it if yours is older) and Postgres. sqlc, oapi-codegen and dbmate run through `go run …@version`, so you do not install them.

```sh
export DATABASE_URL="postgres://localhost/mrebay?sslmode=disable"
createdb mrebay
make dev                 # migrate, then serve on :8080
make key NAME=you        # in another terminal: mint an api key (printed once)
```

Open http://localhost:8080, sign in with the key, save a search, and press Run now (with eBay credentials set; see [docs/deploy.md](docs/deploy.md)). From a script:

```sh
curl -H "Authorization: Bearer mre_…" localhost:8080/api/searches \
  -d '{"name":"rain shell","query":"arcteryx beta","maxPriceCents":20000,"conditions":["USED"],"wants":"mens M"}'
```

Connect Claude Code: `claude mcp add --transport http mr-ebay http://localhost:8080/mcp/rpc --header "Authorization: Bearer mre_…"`. Prices are integer cents everywhere in the API, and 0 means no limit.

## Layout

```
api/openapi.yaml        the HTTP API's contract (and which operations are public)
cmd/server/             the binary: serve, keys, invites
db/migrations/          dbmate migrations
db/queries/             sqlc queries
deploy/                 sprite scripts: run-postgres.sh, redeploy.sh
docs/                   how it works and why
internal/app/           Store, Links, shared errors and the app's name
internal/auth/          key/token formats, middleware, who-is-calling context
internal/oauth/         the OAuth 2.1 server claude.ai needs
internal/server/        Endpoint contract, the composed REST server, one package per resource
internal/appmcp/        MCP tools over the same endpoints
internal/ebay/          the eBay client (token, search); ebaytest/ is a fake eBay for tests
internal/web/           the router, the pages (templates/), end-to-end tests
internal/api/gen/       oapi-codegen output: do not edit
internal/db/gen/        sqlc output: do not edit
```

## Make targets

| target | what it does |
| --- | --- |
| `make dev` | migrate, then run the server on :8080 |
| `make generate` | regenerate sqlc and oapi-codegen output after any SQL, migration or spec change |
| `make check` | fail if the generated code is stale (CI runs it) |
| `make migrate` / `make rollback` | dbmate up / roll back the last migration |
| `make new-migration NAME=…` | create a timestamped migration file |
| `make key NAME=…` | mint an api key |
| `make invite NOTE=…` | mint a single-use invite |
| `make build` | compile to `bin/server` |
| `make test` | run the tests; set `TEST_DATABASE_URL` to a scratch database **the tests may wipe** to include the end-to-end ones |

## Docs

- [Design](docs/design.md): what it does, the schema, the eBay integration and the build order
- [Architecture](docs/architecture.md): the layers, the request flow, and why it is built this way
- [Adding a resource](docs/adding-a-resource.md): the end-to-end recipe
- [Auth](docs/auth.md): keys, OAuth, invites, sessions, and public versus private
- [Deploy](docs/deploy.md): the sprites.dev recipe and CI redeploys
- [CLAUDE.md](CLAUDE.md): orientation for Claude Code sessions
