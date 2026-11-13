<p align="center">
  <img src="https://raw.githubusercontent.com/VizzleTF/alerse/main/artwork/alerse_256.png" width="120" alt="The Alerse icon">
</p>

<h1 align="center">Alerse Sync</h1>

<p align="center">The sync server of Alerse: end-to-end encrypted, no account, run by you.</p>

The sync server of [Alerse](https://github.com/VizzleTF/alerse) (called Arcomium up to
0.1.7; the binary and Go module keep the name arcomium-sync, the image is
`ghcr.io/vizzletf/alerse-sync` from 0.1.3 on, and its protocol identifiers never
change). It syncs
bookmarks, tabs, history, passwords and settings between your devices and tells
them about changes right away. You run it yourself, for one household or more;
it needs only PostgreSQL. A free public server is planned.

- **The 24 words and a password.** A new device needs both: the words alone
  open nothing, and a copy of the server's database cannot be used to guess the
  password without the words. The server holds the account key only as the
  device encrypted it.
- **A key per device**, so a lost device can be removed and is refused from its
  next request on.
- **End-to-end encrypted** when the chain sets a passphrase: the server stores
  data it cannot read, and can be told to refuse anything else.
- **Real-time.** A device that commits a change wakes the chain's other devices
  over Server-Sent Events.
- **Speaks Chromium's sync protocol**, so every data type Chromium syncs works,
  including ones added later.

## Run your own server

With Docker, on a host that a domain name points to, with ports 80 and 443
open:

```sh
curl -O https://raw.githubusercontent.com/VizzleTF/alerse-sync/main/docker-compose.yml
SYNC_DOMAIN=sync.example.com POSTGRES_PASSWORD=change-me docker compose up -d
curl https://sync.example.com/health    # prints "ok"
```

Caddy gets the certificate and forwards to the server, which is not exposed
otherwise. Then set Alerse's sync server to `https://sync.example.com`.

Without a domain, on a home network or behind Tailscale, remove the `caddy`
service from `docker-compose.yml` and publish the server's port as the file
describes. Plain HTTP is fine on loopback or a private network, never over the
internet. A Cloudflare Tunnel also works; set `TRUSTED_PROXY` to the tunnel's
address and `CLIENT_IP_HEADER=CF-Connecting-IP`.

On Kubernetes, use the manifests in `deploy/kubernetes`:
[Run alerse-sync on Kubernetes](docs/kubernetes.md) walks through them.

Without Docker, take the binary for your system from
[Releases](https://github.com/VizzleTF/alerse-sync/releases) and point it at a
Postgres database:

```sh
DATABASE_URL=postgres://user:password@host:5432/arcomium ./arcomium-sync
```

The server creates and updates its tables on start.

## Configuration

Everything is set with environment variables. Sizes take `KiB`, `MiB` or
`GiB` (`256MiB`); durations take Go units or days (`90s`, `2h`, `30d`). A limit
or duration of `0` turns it off.

**Server**

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | required | Postgres URL. `pool_max_conns=N` in it sets the pool size; otherwise it is `MAX_CONCURRENT_REQUESTS` up to 32, plus 4 |
| `PORT` | `8295` | HTTP port |
| `METRICS_ADDR` | off | Address for Prometheus metrics at `/metrics`, such as `127.0.0.1:9295`. Keep it off the internet |
| `ACCESS_LOG` | `true` | Log every request: method, path, status, time, sizes, a shortened chain ID. Never bodies, query strings, tokens or IP addresses |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; logs are JSON on stderr |
| `SHUTDOWN_DELAY` | `0` | How long to fail `/readyz` before stopping on SIGTERM, so a load balancer stops sending requests first |
| `SESSIONS_NUDGE_DELAY` | `3s` | How long a device waits before it sends a change to its open tabs; `0` leaves Chromium's 15 seconds |

**What a sync chain may store**

| Variable | Default | Meaning |
|---|---|---|
| `MAX_BYTES_PER_CHAIN` | `256MiB` | Size of everything a chain stores, deleted entities included |
| `MAX_ENTITIES` | `250000` | Bookmarks, passwords, settings and the other types not below |
| `MAX_HISTORY_ENTITIES` | `100000` | History visits and history deletions. Beyond this the oldest visits are dropped instead of refusing new ones |
| `MAX_HISTORY_BYTES` | `64MiB` | Size of history, handled the same way. History also gives way when other data needs room |
| `MAX_SESSION_ENTITIES` | `20000` | Open tabs and windows of all the chain's devices |
| `REQUIRE_CUSTOM_PASSPHRASE` | `false` | Refuse data that is not end-to-end encrypted: until a device sets a sync passphrase, only device info and the encryption settings sync |

**Abuse limits**

| Variable | Default | Meaning |
|---|---|---|
| `RATE_LIMIT_PER_MINUTE` | `600` | Requests per sync chain |
| `RATE_LIMIT_PER_IP_PER_MINUTE` | `1200` | Requests per client address (IPv6: per /64) |
| `NEW_CHAINS_PER_IP_PER_DAY` | `10` | Sync chains one address may create |
| `NEW_ACCOUNTS_PER_IP_PER_DAY` | `10` | Accounts one address may create |
| `UNLOCK_ATTEMPTS_PER_HOUR` | `10` | Password checks per account, right or wrong: sign-ins and removing a device or the account |
| `UNLOCK_ATTEMPTS_PER_IP_PER_HOUR` | `30` | Password checks of accounts per client address |
| `UNLOCK_LOOKUPS_PER_IP_PER_HOUR` | `120` | Account sign-in parameter lookups per client address |
| `ACCOUNT_CHANGES_PER_HOUR` | `120` | Account and device requests per account |
| `MAX_ACCOUNTS` | `0` | Accounts the server keeps |
| `MAX_DEVICES_PER_ACCOUNT` | `50` | Signed-in devices per account |
| `MAX_CHAINS_PER_ACCOUNT` | `50` | Sync chains per account, one per Space |
| `MAX_CHAINS` | `0` | Sync chains the server keeps |
| `MAX_CONCURRENT_REQUESTS` | `64` | Protocol requests handled at once, which bounds memory |
| `BODY_READ_TIMEOUT` | `20s` | Time a client has to send a request body |
| `MAX_EVENT_STREAMS` | `32` | Open event streams per sync chain on one instance |
| `MAX_EVENT_STREAMS_PER_IP` | `64` | Open event streams per client address on one instance |
| `MAX_EVENT_STREAMS_TOTAL` | `10000` | Open event streams on one instance |
| `TRUSTED_PROXY` | none | Comma-separated networks of reverse proxies, such as `10.0.0.0/8,::1`. Only from them is `CLIENT_IP_HEADER` believed |
| `CLIENT_IP_HEADER` | `X-Forwarded-For` | Header that names the client; `CF-Connecting-IP` or `X-Real-IP` work too |

**Retention**

| Variable | Default | Meaning |
|---|---|---|
| `TOMBSTONE_TTL` | `60d` | How long a deleted entity's tombstone is kept. A device offline for longer downloads its data again |
| `HISTORY_TTL` | `90d` | How long history is kept after it was last written; Chromium keeps 90 days and leaves expiry to the server |
| `EMPTY_CHAIN_TTL` | `7d` | Removes chains no device ever committed to |
| `INACTIVE_CHAIN_TTL` | `0` | Removes chains no device contacted for this long |
| `REMOVED_DEVICE_TTL` | `90d` | How long the record of a removed device is kept; meanwhile its key cannot join again |
| `RETENTION_INTERVAL` | `1h` | How often the retention job runs |

Several instances can share one database behind a load balancer: events reach
devices on every instance through Postgres `NOTIFY`, and the retention job runs
on one instance at a time.

### A public server

Anyone who reaches the server can create a sync chain. On a server open to the
internet, also set:

```sh
MAX_CHAINS=10000
REQUIRE_CUSTOM_PASSPHRASE=true   # the operator never holds readable data
INACTIVE_CHAIN_TTL=365d          # say so to your users
TRUSTED_PROXY=...                # or the per-address limits count the proxy
METRICS_ADDR=127.0.0.1:9295
```

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v3/chains/{chain}/command/` | Chromium sync protocol, for a chain of an account |
| `GET /v3/chains/{chain}/events` | Change events of a chain of an account |
| `/v3/account`, `/v3/unlock` | Accounts with the 24 words and a password: create, sign a device in, delete |
| `/v3/devices` | An account's devices: list, remove |
| `GET /health` | `200 ok` while the database answers, `503` otherwise |
| `GET /livez` | `200` while the process runs |
| `GET /readyz` | `200` while the server takes requests; `503` when the database is down or it is shutting down |
| `GET /metrics` | Prometheus metrics, on `METRICS_ADDR` only |

Devices authenticate with a token signed by their own key; creating an account
and signing a device in need no token. The token format, the derivations, the
events, the limits and what the server keeps are in
[How the server works](docs/protocol.md). Releases up to 0.1.5 also served
`/v2` (chains without accounts, sign-in with a login name, pairing); 0.2.0
removed it.

## Operate

**Health checks.** The image has no shell or curl; `arcomium-sync healthcheck`
exits 0 when `/readyz` on `PORT` answers 200. The image and
`docker-compose.yml` use it.

**Shutdown.** On SIGTERM the server fails `/readyz`, waits `SHUTDOWN_DELAY`,
closes event streams (devices reconnect) and gives requests in progress up to
30 seconds to finish.

**Metrics** include requests by message and result, latency, commit answers,
requests refused by each limit, history dropped for room, what the retention
job removed, open event streams and database pool use.

**Backups.** Everything is in Postgres. A nightly dump is enough for a small
server:

```sh
docker compose exec -T postgres pg_dump -U arcomium -Fc arcomium > arcomium-$(date +%F).dump
```

For a public server, archive the write-ahead log for point-in-time recovery,
with pgBackRest or WAL-G.

## Develop

Requires Go 1.27.

```sh
go test ./...                              # unit tests
go test -p 1 -tags integration ./...       # also on Postgres, which the tests download and start
go test -fuzz FuzzHandle ./internal/protocol/
go run ./cmd/arcomium-sync                 # needs DATABASE_URL
```

CI runs the tests with `-race` on Postgres, `govulncheck` and golangci-lint. Releases
build the multi-arch image with an SBOM and build provenance, from base images and
actions pinned by digest, and sign it with Sigstore. To check an image before you run
it:

```sh
cosign verify ghcr.io/vizzletf/alerse-sync:0.1.5   --certificate-identity-regexp '^https://github.com/VizzleTF/alerse-sync/.github/workflows/release.yml@refs/tags/v'   --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The protocol definitions in `proto/` come from Chromium. After moving Alerse
to a new Chromium version, refresh them with `scripts/update-protos.sh
<chromium src>` (needs `protoc` and `protoc-gen-go`).

## License

BSD-3-Clause. The files in `proto/` are Chromium's, under
[its license](proto/LICENSE).
