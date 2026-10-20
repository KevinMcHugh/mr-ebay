# Design

Mr. eBay keeps your eBay shopping in one place: what you are looking for (**searches**), what eBay has that fits (**listings**, and the **matches** linking them to searches), and how prices move (**price points**). It has two modes:

- **On demand.** You ask Claude to find something. Claude runs a search here, reads the results, and judges them against what you said you want.
- **Watching.** A search marked `watch` runs unattended. New matches, and price drops at or below its alert threshold, become alerts for Claude to report.

The server does the mechanical work: calling eBay, de-duplicating, tracking prices, remembering what you have already seen. Claude does the judgment calls, such as "is this actually a medium?" or "does this seller look sketchy?". The server never calls a model (see [architecture.md](architecture.md)).

## Status

| piece | state |
| --- | --- |
| Schema for searches, listings, matches, price points, runs | **done** (`db/migrations/20261020000001_ebay.sql`) |
| Saved searches: REST, MCP tools, pages, tests | **done** (`internal/server/searches`) |
| eBay client | planned, step 1 below |
| Ad-hoc search, running a saved search, matches | planned, steps 2 and 3 |
| Listing pages with photos and price history | planned, step 4 |
| Watching and alerts | planned, step 5 |

## Data model

| table | holds |
| --- | --- |
| `searches` | keywords plus eBay filters (categories, conditions, buying options, price range, currency, marketplace), `wants` (the rest in words), `watch`, `alert_below_cents`, `last_run_at` |
| `listings` | one row per eBay item, keyed by `ebay_item_id`: title, URL, image, seller, condition, price and shipping in cents, end time, `first_seen_at`/`last_seen_at`, and `data` (the item summary exactly as eBay returned it) |
| `matches` | which search found which listing, with a per-search `status` (`new` → `seen`, `starred` or `dismissed`) and `alerted_at` |
| `price_points` | append-only: a row whenever a run sees a listing at a different total |
| `search_runs` | append-only: each run's trigger (`manual` or `watch`), counts, and error |

Decisions:

- **A listing is stored once**, however many searches find it. Status lives on the match, because dismissing a jacket from "rain shell" should not hide it from "anything Arc'teryx".
- **Money is integer cents plus a currency.** Never floats. eBay sends prices as decimal strings, so parse them straight into cents.
- **`data` keeps eBay's raw item summary**, so a field we did not copy into a column is never lost. This is Creature Lab's "store it as it was rolled" rule.
- **Price points and runs are append-only history.** They have no `updated_at` or `deleted_at`, and they go away with their parent. This follows inventory's precedent for derived tables (`indexed_fields`).
- **A listing that drops out of results is not "gone".** Search results shift with relevance and paging. Use `ends_at`, or a later per-item lookup, to decide that a listing has ended.

## eBay integration (step 1)

Use the **Browse API**'s `item_summary/search`. It takes an application token from the OAuth client-credentials grant, which needs a developer account at developer.ebay.com and no user sign-in. Check these details against eBay's current docs while building; they are from memory:

- Token: `POST https://api.ebay.com/identity/v1/oauth2/token` with basic auth `client_id:client_secret`, `grant_type=client_credentials`, `scope=https://api.ebay.com/oauth/api_scope`. Cache it until shortly before `expires_in`.
- Search: `GET https://api.ebay.com/buy/browse/v1/item_summary/search?q=…&category_ids=…&filter=…&limit=…`, with the header `X-EBAY-C-MARKETPLACE-ID: EBAY_US`. The filter is a comma-separated list such as `price:[10..200],priceCurrency:USD,conditions:{USED},buyingOptions:{FIXED_PRICE|BEST_OFFER}`.
- Mind the daily call quota. A run fetches at most a few pages (say 200 items), and watching runs a few times a day, not every minute.
- **Sold prices are not in the Browse API.** As far as I know they come from the Marketplace Insights API, which eBay restricts. Until then, the price history here is asking prices over time, not what things sold for.

Shape: `internal/ebay` holds a `Client` interface (`Search(ctx, Query) ([]Item, error)`) with an HTTP implementation configured from `EBAY_CLIENT_ID`, `EBAY_CLIENT_SECRET` and an overridable base URL, so tests point it at an `httptest` server that serves canned JSON. Endpoints take the client the way they take a Store: as a narrow interface field. The REST server, MCP and pages get the same one from `web.New`. Without credentials the app still runs, and the run endpoints return a clear "eBay is not configured" error.

## Running a search (steps 2 and 3)

`RunEndpoint` (`POST /api/searches/{id}/run`, MCP `run_search`):

1. Build the eBay query from the search's columns, then fetch.
2. In one transaction, per item: upsert the listing by `ebay_item_id`, refreshing its columns, `data` and `last_seen_at`. Insert a price point if the total (price plus shipping) differs from the latest one. Upsert the match, which is `new` on first sight.
3. Record a `search_runs` row with its counts, or with its error if eBay failed, and set `searches.last_run_at`.
4. Return the run, the new matches, and price drops.

`search_ebay` (MCP) and `GET /api/ebay/search` run an ad-hoc query and **store nothing**, like Creature Lab's `preview_species`. They let Claude try queries before saving one.

## Watching and alerts (step 5)

A sprite pauses when idle, and a paused sprite's timers do not fire. So a watcher on the sprite cannot wake itself. Something outside has to call in:

- **Recommended: a scheduled Claude task**, say each morning, with the Mr. eBay connector. It calls `run_watched_searches`, then `list_alerts`, judges each alert against its search's `wants`, tells you about the ones worth it (the Claude app can push to your phone), and calls `ack_alerts`. It is the waker and the judge in one, which is the reason `wants` exists. It costs model usage per run.
- **Alternatively, a GitHub Actions cron** that `curl`s `POST /api/runs/watched` with an api key. This uses no model, but nothing judges the results, so alerts wait until you next ask Claude, or until a notification channel (an email, ntfy.sh) is added.

An alert is a match that is new or whose price dropped, with a total at or below the search's `alert_below_cents` (any total when that is NULL), and `alerted_at` still NULL. `ack_alerts` sets `alerted_at`, so a match alerts once per change.

## MCP tools

| tool | does | state |
| --- | --- | --- |
| `whoami` | the api key the connection acts as | done |
| `list_searches`, `get_search` | saved searches, with unread match counts | done |
| `create_search`, `update_search`, `delete_search` | manage them | done |
| `search_ebay` | ad-hoc eBay query; stores nothing | planned |
| `run_search` | run one saved search now; returns new matches and price drops | planned |
| `run_watched_searches` | run every watched search (the scheduled task calls this) | planned |
| `list_matches` | a search's matches, filtered by status, newest first | planned |
| `set_match_status` | mark matches seen, starred or dismissed | planned |
| `get_listing` | one listing with its price history and which searches found it | planned |
| `list_alerts`, `ack_alerts` | what a watcher should report, and marking it reported | planned |
| `list_runs` | recent runs and their errors: "is the watcher actually working?" | planned |

Each one gets the matching REST operation in `api/openapi.yaml` and drives the same endpoint, as the search tools do.

## Pages (step 4)

The search page grows a grid of matches (photo, title, total, condition, seller, ends) with seen, star and dismiss buttons, and a listing page with its price history as a plain table. eBay's images are served from `i.ebayimg.com`, so add that host to `img-src` in `pageHeaders`' CSP. Still no script.

## Using it with inventory

They stay separate apps (see the README for why). Claude joins them in conversation: read a wardrobe item from inventory, then write a search here whose `wants` says what it must match. Neither app calls the other.

## Build order

1. `internal/ebay`: token and search, against a fake server in tests.
2. `search_ebay` and its REST operation (stores nothing).
3. `run_search`, matches, `set_match_status`, `get_listing`, `list_runs`.
4. Match grid and listing pages.
5. `run_watched_searches`, alerts, and the scheduled task's prompt, which goes in this doc.
