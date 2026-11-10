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

- **No account.** Devices join a sync chain with its sync code. The server
  knows the chain by a key derived from the code, never the code itself.
- **Optional sign-in.** A chain may add a login name and password, so new
  devices can join without typing the code. The device encrypts the code with
  a key from the password; the server never sees the login name, the password
  or the code.
- **Pairing.** A device that syncs shows a short one-time code; a new device
  types it and gets the sync code over a SPAKE2 exchange the server only
  relays.
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
otherwise. Then set Alerse's (or Arcomium's) sync server to `https://sync.example.com/v2`.

Without a domain, on a home network or behind Tailscale, remove the `caddy`
service from `docker-compose.yml` and publish the server's port as the file
describes. Plain HTTP is fine on loopback or a private network, never over the
internet. A Cloudflare Tunnel also works; set `TRUSTED_PROXY` to the tunnel's
address and `CLIENT_IP_HEADER=CF-Connecting-IP`.

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
| `LOGIN_ATTEMPTS_PER_HOUR` | `10` | Sign-in attempts per login name, right or wrong |
| `LOGIN_ATTEMPTS_PER_IP_PER_HOUR` | `30` | Sign-in attempts per client address |
| `LOGIN_LOOKUPS_PER_IP_PER_HOUR` | `120` | Sign-in parameter lookups per client address |
| `LOGIN_CHANGES_PER_HOUR` | `30` | Sign-in reads and changes per sync chain |
| `PAIR_CODES_PER_HOUR` | `20` | Pairing codes a sync chain may open |
| `PAIR_JOINS_PER_IP_PER_HOUR` | `30` | Pairing joins per client address |
| `PAIR_READS_PER_IP_PER_HOUR` | `1200` | Pairing slot reads per client address; a waiting device reads every 2 seconds |
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
| `POST /v2/command/` | Chromium sync protocol |
| `GET /v2/events` | Change events of a sync chain |
| `/v2/login` | Optional sign-in with a login name and password: set, change, remove, and join with it |
| `/v2/pair` | Relay for pairing a new device with a short code |
| `GET /health` | `200 ok` while the database answers, `503` otherwise |
| `GET /livez` | `200` while the process runs |
| `GET /readyz` | `200` while the server takes requests; `503` when the database is down or it is shutting down |
| `GET /metrics` | Prometheus metrics, on `METRICS_ADDR` only |

Clients authenticate the `/v2` endpoints with a signed token, except the
requests of a device that joins with a sign-in or a pairing code. The token
format, the events, the sign-in, pairing, the limits and what the server keeps
are in
[How the server works](docs/protocol.md).

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
actions pinned by digest.

The protocol definitions in `proto/` come from Chromium. After moving Alerse
to a new Chromium version, refresh them with `scripts/update-protos.sh
<chromium src>` (needs `protoc` and `protoc-gen-go`).

## License

BSD-3-Clause. The files in `proto/` are Chromium's, under
[its license](proto/LICENSE).
