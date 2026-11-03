# Arcomium Sync

The sync server of [Arcomium](https://github.com/VizzleTF/arcomium). It syncs
bookmarks, tabs, history, passwords and settings between your devices and tells
them about changes right away. You can use the public server or run your own;
it needs only PostgreSQL.

- **No account.** Devices join a sync chain with its sync code. The server
  knows the chain by a key derived from the code, never the code itself.
- **End-to-end encrypted** when the chain sets a passphrase: the server stores
  data it cannot read.
- **Real-time.** A device that commits a change wakes the chain's other devices
  over Server-Sent Events.
- **Speaks Chromium's sync protocol**, so every data type Chromium syncs works,
  including ones added later.

## Run your own server

With Docker:

```sh
curl -O https://raw.githubusercontent.com/VizzleTF/arcomium-sync/main/docker-compose.yml
POSTGRES_PASSWORD=change-me docker compose up -d
curl http://localhost:8295/health    # prints "ok"
```

Without Docker, take the binary for your system from
[Releases](https://github.com/VizzleTF/arcomium-sync/releases) and point it at a
Postgres database:

```sh
DATABASE_URL=postgres://user:password@host:5432/arcomium ./arcomium-sync
```

The server creates and updates its tables on start. Then set Arcomium's sync
server to `http://<host>:8295/v2`. To reach a home server from outside without
opening ports, put it behind Tailscale or a Cloudflare Tunnel; over the
internet, serve it over HTTPS.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | required | Postgres URL. `pool_max_conns=N` in it sets the connection pool size |
| `PORT` | `8295` | HTTP port |
| `RATE_LIMIT_PER_MINUTE` | `600` | Requests per minute per sync chain; `0` turns the limit off |
| `MAX_ENTITIES` | `250000` | Entities one sync chain may store |
| `MAX_CHAINS` | `0` | Sync chains the server keeps; `0` for no limit |
| `MAX_CONCURRENT_REQUESTS` | `64` | Protocol requests handled at once, which bounds memory; `0` for no limit |
| `MAX_EVENT_STREAMS` | `32` | Open event streams per sync chain on one server instance |
| `MAX_EVENT_STREAMS_TOTAL` | `10000` | Open event streams on one server instance |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`; logs are JSON on stderr |

Several instances can share one database behind a load balancer: events reach
devices on every instance through Postgres `NOTIFY`. Anyone who reaches the
server can create a sync chain, so a public server needs `MAX_CHAINS` and a
reverse proxy that limits requests per IP address.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `POST /v2/command/` | Chromium sync protocol |
| `GET /v2/events` | Change events of a sync chain |
| `GET /health` | `200 ok` while the database answers, `503` otherwise |

Clients authenticate both `/v2` endpoints with a signed token. The token format,
the events and what the server keeps are in [How the server works](docs/protocol.md).

## Develop

Requires Go 1.27.

```sh
go test ./...                              # unit tests
go test -tags integration ./e2e/           # downloads and starts its own Postgres
go run ./cmd/arcomium-sync                 # needs DATABASE_URL
```

The protocol definitions in `proto/` come from Chromium. After moving Arcomium
to a new Chromium version, refresh them with `scripts/update-protos.sh
<chromium src>` (needs `protoc` and `protoc-gen-go`).

## License

BSD-3-Clause. The files in `proto/` are Chromium's, under
[its license](proto/LICENSE).
