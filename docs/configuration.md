# Configuration and secrets

**The rule: secrets live in Sprites connectors, and everything else reaches the app as environment variables on its sprite service** (`sprite-env services create … --env`). No env files: none on the sprite, none in the repo, and no code that loads one (no dotenv). No secret on the service's environment either. This is [app-template](https://github.com/KevinMcHugh/app-template)'s rule, and its `docs/configuration.md` has the full reasoning.

## The eBay keyset: a Sprites connector

A [Sprites connector](https://docs.sprites.dev/concepts/connectors/) keeps a credential in the Sprites control plane, encrypted, and adds it to requests on their way out. The app calls `https://api.sprites.dev/v1/gateway/custom_api/<connection id>/<path>` with no credential. The gateway knows which sprite is calling from its platform identity, checks the connector's access policy (which sprites, and which paths), and forwards to the provider with the credential attached. The secret is never on the sprite, so nothing there can read it, log it, or copy it into a checkpoint, and rotating it needs no redeploy.

There is no eBay provider in the connector catalog, so this uses the **Custom API** connector, which holds one token and sends it in one header to one base URL. That is enough here, because the keyset is used in exactly one place: the client-credentials request to eBay's token endpoint, which takes `client_id:client_secret` as HTTP Basic. So the connector holds `base64(id:secret)`, sends it as `Authorization: Basic …`, and forwards only that path. It returns an application token that lasts two hours. The app keeps that token in memory and sends Browse API calls straight to eBay with it, as before.

| | production | sandbox |
| --- | --- | --- |
| connector base URL | `https://api.ebay.com` | `https://api.sandbox.ebay.com` |
| the one path it forwards | `/identity/v1/oauth2/token` | `/identity/v1/oauth2/token` |
| the app's settings | `EBAY_TOKEN_URL` | `EBAY_TOKEN_URL`, `EBAY_ENV=sandbox` |

Create it, or rotate the keyset, on your own machine:

```sh
SPRITES_TOKEN=… deploy/connector.sh mr-ebay-ebay https://api.ebay.com/identity/v1/oauth2/token mcp-mr-ebay
```

It asks for the App ID (client id) and Cert ID (client secret) without echoing them, sends them to the Sprites API on stdin, and prints the token URL to put on the service's environment. Running it again with the same name replaces the keyset in place. Only sprites whose name starts with `mcp-mr-ebay` may use the connector, and only for that one path. `SPRITES_TOKEN` is the token from [sprites.dev/account](https://sprites.dev/account), the same kind CI deploys with.

Run it yourself, not through Claude: a secret typed into a conversation has left your hands. Claude can do everything around it, and check what the sprite can reach with `curl -sS https://api.sprites.dev/v1/gateway/list`, which names the connectors the sprite may use and never shows a credential.

## What the app reads

| variable | used by | notes |
| --- | --- | --- |
| `DATABASE_URL` | `cmd/server` (serve, keys, invites), `make migrate` | on a sprite: `postgres:///<app>?host=/var/run/postgresql&sslmode=disable` (local socket, no password) |
| `PORT` | `cmd/server serve` | default `8080`; must match the service's `--http-port` |
| `PUBLIC_URL` | `cmd/server serve` | the app's https origin; OAuth discovery advertises it, so it must be exact |
| `EBAY_TOKEN_URL` | `ebay.FromEnv`, called by `cmd/server serve` | on the sprite: the eBay connector's token URL, which `deploy/connector.sh` prints. Unset, with no keyset either, the app runs and answers every eBay call with "eBay is not configured" |
| `EBAY_CLIENT_ID`, `EBAY_CLIENT_SECRET` | `ebay.FromEnv` | **off a sprite only**: local development, where there is no gateway. Never set on the sprite, because the connector holds them |
| `EBAY_ENV` | `ebay.FromEnv` | `sandbox` for the sandbox; anything else is production. Must match the connector's base URL |
| `EBAY_API_BASE` | `ebay.FromEnv` | overrides the eBay host entirely; never set in production |
| `TEST_DATABASE_URL` | tests only | a scratch database the end-to-end tests may wipe; never set on a sprite |

Read settings once, at startup, in `cmd/server/main.go`, or in a `FromEnv` constructor that main calls (`ebay.FromEnv`), and pass the values down. Packages below main never call `os.Getenv`, so tests can configure them directly. An optional integration that is not configured should leave the app running and say what is missing, not crash it.

The client sends HTTP Basic only when it holds the secret. Tests run it as it runs on the sprite: `ebaytest.Server.Connector` is a fake connector in front of the fake eBay that adds the keyset and fails the test if the client sent a credential of its own. The end-to-end tests use it too.

## Setting and changing values on the sprite

The live sprite is `mcp-mr-ebay` (see [deploy.md](deploy.md)). `services create` replaces the service definition rather than merging into it, so a change means recreating the service with **every** variable:

```sh
sprite exec -s mcp-mr-ebay -- bash -lc '/.sprite/bin/sprite-env services delete mcp-mr-ebay && \
  /.sprite/bin/sprite-env services create mcp-mr-ebay \
    --cmd /home/sprite/mcp-mr-ebay/bin/server --args serve --dir /home/sprite/mcp-mr-ebay \
    --needs postgres --http-port 8080 \
    --env "DATABASE_URL=postgres:///mcp-mr-ebay?host=/var/run/postgresql&sslmode=disable,PORT=8080,PUBLIC_URL=https://mcp-mr-ebay-b2mxg.sprites.app,EBAY_TOKEN_URL=https://api.sprites.dev/v1/gateway/custom_api/conn_…/identity/v1/oauth2/token" \
    --no-stream'
```

The server's startup log no longer warns "eBay is not configured" once the token URL is set.

- `--env` is comma-separated, so no value can contain a comma.
- `redeploy.sh` only restarts the service, so the environment carries across deploys untouched. Rotating the keyset in the connector changes nothing here.
- **Through the Fly.io MCP connector:** its `service_create` tool has no environment option, so Claude runs the same `sprite-env services create … --env …` command through the connector's `exec` instead. Never work around that gap with a file. None of these values is a secret.
- **If the keyset was ever on the service's environment,** recreate the service without it, then rotate it at developer.ebay.com and store the new one with `deploy/connector.sh`, since the old one sat in the sprite's checkpoints.

## Local development

Export variables in the shell you run `make dev` from (`export DATABASE_URL=…`). The gateway answers only sprites, so locally eBay needs a keyset exported (`EBAY_CLIENT_ID`, `EBAY_CLIENT_SECRET`, ideally sandbox keys with `EBAY_ENV=sandbox`), or stays off. Do not create a `.env` file. `.gitignore` still lists `.env*`, but only as a backstop against committing one by accident, not as permission to use one.
