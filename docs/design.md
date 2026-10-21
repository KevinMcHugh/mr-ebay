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
| eBay client and a fake eBay for tests | **done** (`internal/ebay`, `internal/ebay/ebaytest`); not yet called by any endpoint |
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

## eBay integration (step 1, done)

`internal/ebay` uses the **Browse API**'s `item_summary/search`, with an application token from the OAuth client-credentials grant. That needs a developer account at developer.ebay.com and no user sign-in.

- **Token:** `POST {base}/identity/v1/oauth2/token`, with basic auth `client_id:client_secret`, `grant_type=client_credentials` and `scope=https://api.ebay.com/oauth/api_scope`. The client caches the token and renews it five minutes before `expires_in`. If eBay answers 401 anyway, the client drops the token and retries once.
- **Search:** `GET {base}/buy/browse/v1/item_summary/search` with `q`, `category_ids`, `filter`, `limit` (at most 200) and `offset`, plus the header `X-EBAY-C-MARKETPLACE-ID`. A `Search` pages until it has `MaxItems` (default 200, capped at 1000) or eBay runs out.
- **Filter:** `ebay.Filter` builds strings like `buyingOptions:{AUCTION|FIXED_PRICE},conditions:{USED},price:[10.00..200.00],priceCurrency:USD`. A minimum alone is `price:[10.00]` and a maximum alone is `price:[..50.00]`.
- **"Any buying option" is sent explicitly.** With no `buyingOptions` filter, eBay returns fixed-price listings only, so a search that leaves buying options empty sends all three.
- **One category per request.** A search with several categories runs once per category and merges the results, keeping an item's first sighting. That costs a call per category, and it is correct whether or not eBay accepts several categories in one request.
- **Mapping an item:** prices arrive as decimal strings, and `ebay.Cents` parses them without going through a float. An auction with no Buy It Now is priced at `currentBidPrice`. Shipping is the first option's `shippingCost`; when there is none (calculated shipping), it is unknown (`nil`), not free. `feedbackPercentage` is a string, and `itemEndDate` is RFC 3339. The whole summary is kept in `Item.Raw`, which becomes `listings.data`.
- **Errors:** eBay's `{"errors":[…]}` bodies become an `*ebay.APIError` with their messages. A 429 is `ebay.ErrRateLimited`. Missing credentials give `ebay.ErrNotConfigured`, so the app runs without them, and endpoints will turn that into a clear message.
- **Config:** `ebay.FromEnv` reads `EBAY_CLIENT_ID` and `EBAY_CLIENT_SECRET`, plus `EBAY_ENV=sandbox` for the sandbox host or `EBAY_API_BASE` to point it anywhere else.
- **Tests:** `ebaytest.New(t)` is a fake eBay (token and search) with eBay's response shapes, paging and error bodies. It records every request so tests can check the filter sent, and it can revoke a token or fail with a status. Later endpoint and end-to-end tests use the same fake.
- **Quota:** a run fetches one page (200 items) by default, and watching should run a few times a day, not every minute.

**What was checked, and what was not.** The response field names (`itemSummaries`, `price.value` as a string, `currentBidPrice`, `shippingOptions[].shippingCost`, `seller.feedbackPercentage`, `itemEndDate`) and the fixed-price-only default were checked against published descriptions of the API. eBay's own developer site could not be reached from the build environment, so the token URL, the marketplace header and the price-range syntax are from memory. **Run one real search with sandbox or production credentials before relying on it.**

**Sold prices are not in the Browse API.** As far as I know they come from the Marketplace Insights API, which eBay restricts. So the price history here is asking prices over time, not what things sold for.

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

1. ~~`internal/ebay`: token and search, against a fake server in tests.~~ Done.
2. `search_ebay` and its REST operation (stores nothing).
3. `run_search`, matches, `set_match_status`, `get_listing`, `list_runs`.
4. Match grid and listing pages.
5. `run_watched_searches`, alerts, and the scheduled task's prompt, which goes in this doc.
