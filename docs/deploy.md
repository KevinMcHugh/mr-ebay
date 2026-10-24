# Deploy (sprites.dev)

The app runs on a [sprites.dev](https://sprites.dev) sprite: a small persistent VM that pauses when idle and wakes on a request. Postgres runs on the same sprite as a supervised service. The binary is built there from a git checkout, and CI redeploys by running one script over `sprite exec`.

The recipe below uses `myapp` for the sprite, the service, the checkout directory and the database. They share a name on purpose: `deploy/redeploy.sh` and the CI workflow derive all four from it.

## First deploy

```sh
APP=myapp
REPO_URL=https://github.com/you/myapp.git

sprite create --skip-console $APP
sprite use $APP
sprite exec -- bash -lc 'sudo apt-get update -qq && sudo apt-get install -y -qq postgresql postgresql-contrib git'
sprite exec -- bash -lc 'sudo curl -fsSL -o /usr/local/bin/dbmate \
  https://github.com/amacneil/dbmate/releases/latest/download/dbmate-linux-amd64 && sudo chmod +x /usr/local/bin/dbmate'
sprite exec -- bash -lc "cd ~ && git clone $REPO_URL $APP"

# Postgres as a sprite service. The launcher recreates /var/run/postgresql,
# which is tmpfs on the sprite image and is wiped on every reboot. Set
# PG_VERSION in the service env if apt installed something other than 18.
sprite exec -- bash -lc "sudo install -m 0755 ~/$APP/deploy/run-postgres.sh /usr/local/bin/run-postgres.sh"
sprite exec -- bash -lc '/.sprite/bin/sprite-env services create postgres --cmd /usr/local/bin/run-postgres.sh --no-stream'
sprite exec -- bash -lc "sudo -u postgres createuser -s sprite; sudo -u postgres createdb -O sprite $APP"

DSN="postgres:///$APP?host=/var/run/postgresql&sslmode=disable"
# Config goes on the service (--env) and nowhere else: no env files, no
# exports in ~/.profile. See docs/configuration.md. One-off commands get
# DATABASE_URL inline.
sprite exec -- bash -lc "cd ~/$APP && DATABASE_URL='$DSN' dbmate --migrations-dir db/migrations --no-dump-schema up && go build -o bin/server ./cmd/server"
sprite exec -- bash -lc "DATABASE_URL='$DSN' ~/$APP/bin/server keys create --name you"   # SAVE THE KEY

# PUBLIC_URL must be the https URL, because OAuth discovery advertises it.
sprite exec -- bash -lc "/.sprite/bin/sprite-env services create $APP \
  --cmd /home/sprite/$APP/bin/server --dir /home/sprite/$APP \
  --needs postgres --http-port 8080 \
  --env 'DATABASE_URL=$DSN,PORT=8080,PUBLIC_URL=https://$APP-<org>.sprites.app,EBAY_CLIENT_ID=…,EBAY_CLIENT_SECRET=…' \
  --no-stream"
sprite config update -s $APP --url-auth public
```

`EBAY_CLIENT_ID` and `EBAY_CLIENT_SECRET` are the production keyset from developer.ebay.com. They go in the service's `--env` like everything else, never in a file ([configuration.md](configuration.md)). Add `EBAY_ENV=sandbox` to use sandbox keys. Without them the app still runs, but cannot search eBay.

The sprite needs the Go version in `go.mod`. `go build` fetches that toolchain itself if the installed Go is older.

For someone else, mint an invite they paste when connecting Claude: `sprite exec -s myapp -- bash -lc '~/myapp/bin/server invites create --note ana --expires-in 168h'`.

## Deploying through the Fly.io MCP connector

Claude can do the first deploy itself with the Fly.io connector, as it did for the live instance (sprite `mcp-mr-ebay`, `https://mcp-mr-ebay-b2mxg.sprites.app`). Two things differ from the CLI recipe above:

- **Names.** The connector can only create and reach sprites whose names start with its grant's prefix (`mcp-`), so the sprite, its service, the checkout (`~/mcp-mr-ebay`) and the database are all named `mcp-mr-ebay`. `redeploy.sh` derives all four from the checkout's directory name, so nothing else needs changing.
- **Config.** The connector's `service_create` has no environment option, so the service is created from inside the sprite, through the connector's `exec`, with `sprite-env services create mcp-mr-ebay --cmd /home/sprite/mcp-mr-ebay/bin/server --args serve --dir /home/sprite/mcp-mr-ebay --needs postgres --http-port 8080 --env 'DATABASE_URL=…,PORT=8080,PUBLIC_URL=…'`. To change a value, delete the service and create it again with the new `--env`. `--env` is comma-separated, so no value may contain a comma. **No env files:** an earlier version of this setup sourced config from a dotfile in the home directory, and that was removed. Config lives on the service definition and nowhere else. eBay credentials go in the same `--env` ([configuration.md](configuration.md) has the command); they are not set yet.

What the connector cannot do is change the sprite's URL auth. A new sprite's URL is private (`auth: sprite`), and claude.ai cannot reach it until someone with the CLI runs `sprite config update -s mcp-mr-ebay --url-auth public` once.

## Redeploys

A push to `main` redeploys, once CI is set up:

1. In the GitHub repo's Settings → Secrets and variables → Actions, add the **variable** `SPRITE_NAME` (here `myapp`).
2. Create a token at [sprites.dev/account](https://sprites.dev/account) for the org that owns the sprite, and save it as the **secret** `SPRITES_TOKEN`.

Until `SPRITE_NAME` is set, the deploy job is skipped and only the tests run. To redeploy without a push, use "Run workflow" on the Actions tab, which works from the GitHub mobile app. By hand, the script is the whole redeploy: `sprite exec -s myapp -- bash -lc '~/myapp/deploy/redeploy.sh'`.

`deploy/redeploy.sh`:

1. fast-forwards the checkout to its branch on `origin` (whatever it has checked out; set `BRANCH` to override) and re-runs the pulled copy of itself, so a change to the deploy steps applies in the same deploy;
2. takes a sprite checkpoint named for the commit (`sprite-env checkpoints list`, then `sprite-env checkpoints restore <id>` to go back);
3. reinstalls `run-postgres.sh` and restarts postgres, but only if that file changed;
4. waits for postgres, then runs `dbmate up`;
5. builds `bin/server.new`, swaps it in (keeping `.prev`), and restarts the service;
6. polls `/health` for 30 s. If it never answers, it puts `.prev` back, restarts again, and fails the run.

## Why it is built this way

- **The binary is built on the sprite, not in CI.** Deploying stays "run one script", whether CI or a person runs it, and the binary's VCS stamp matches the checkout it runs from.
- **CI pushes; the sprite does not poll.** A paused sprite's timers do not fire, and keeping it awake to poll costs more than it is worth. `sprite exec` wakes it.
- **Deploys queue and never cancel each other** (`cancel-in-progress: false`). A deploy killed after migrating but before restarting is worse than one that waits.
- **`--ff-only`, not `reset --hard`.** If someone edits the checkout on the sprite, the deploy stops instead of throwing the edit away.
- **The rollback swaps only the binary.** Migrations stay applied, so it helps only when the old binary still runs on the new schema. Write migrations that way: add columns with defaults, and drop things one deploy after the code stops using them.

## Connect Claude

- **claude.ai:** add a custom connector at `https://myapp-<org>.sprites.app/mcp/rpc` with automatic client registration, then paste an `mre_` key when asked, or an `mre_iv_` invite the first time.
- **Claude Code:** `claude mcp add --transport http myapp https://myapp-<org>.sprites.app/mcp/rpc --header "Authorization: Bearer mre_…"`.
