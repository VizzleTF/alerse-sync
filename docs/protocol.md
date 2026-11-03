# How the server works

This page explains what the sync server expects from clients and what it keeps.
It is for people working on Arcomium's sync client or running a server.

## Sync chains and tokens

A sync chain is a set of devices that share data. It has no account: every
device derives the same Ed25519 key pair from the chain's sync code, and the
public key identifies the chain. The server stores the chain under
`hex(SHA-256(public key))`.

Every request carries `Authorization: Bearer <token>`:

```text
token     = base64url, no padding (0x01 | public key | issued at | signature)
issued at = Unix time in milliseconds, 8 bytes big-endian
signature = Ed25519(private key, "arcomium-sync-token-v1" | public key | issued at)
```

The server accepts a token from 24 hours before to 5 minutes after its own
clock. It keeps no session state, so a client can issue a new token for every
request. A bad or expired token gets HTTP 401, which makes Chromium ask for a
new one. `internal/auth` implements this.

## The sync protocol

`POST /v2/command/` takes a `ClientToServerMessage` and answers with a
`ClientToServerResponse`, the messages of Chromium's
`components/sync/protocol/sync.proto`. Request and response bodies may be
gzip-compressed. The server handles three requests:

| Request | What the server does |
|---|---|
| GetUpdates | Returns each requested data type's entities that changed after the client's progress token, oldest first, up to 500 entities or 16 MiB per response. `changes_remaining` tells the client to ask again. With `need_encryption_key` it adds the chain's keystore key |
| Commit | Stores the entities in one transaction and answers each one: `SUCCESS` with its ID and version, `CONFLICT` if the client's version is out of date, `OVER_QUOTA` or `INVALID_MESSAGE` |
| ClearServerData | Deletes the chain. Its other devices get `NOT_MY_BIRTHDAY`, turn sync off and clear their sync data |

**Versions.** Each chain has a counter. Every committed entity gets the next
value as its version, under a lock on the chain, so versions only grow. A
progress token is the highest version a client has of a data type.

**Birthday.** A chain gets a random birthday when it is created. The first
GetUpdates of a device learns it; every later request has to send it back. A
request with another birthday gets `NOT_MY_BIRTHDAY` with the chain's current
birthday, which is how devices find out that the chain was cleared. The server
reports it by `error_code` only: Chromium then turns sync off and clears its
sync data, while an explicit `error` field would leave it stuck.

**New chains.** A GetUpdates for a chain that does not exist creates it with
the entities
Chromium expects on a new account: the bookmark root, Bookmark Bar, Other
Bookmarks, Synced Bookmarks and the Nigori node, which holds the chain's
encryption settings.

**Data types.** The data type of an entity is the field set in its
`EntitySpecifics`. A type newer than the server's copy of the protocol arrives
as an unknown field and syncs like the others. Commit-only types, which clients
never fetch (user events, consents, security events, sharing messages, password
sharing invitations), are accepted and dropped.

**Client tags.** An entity with a `client_tag_hash`, such as a preference, has
one ID per tag. If two devices create it, the second commit conflicts instead
of making a duplicate.

**Limits.** A chain that sends requests too fast gets `THROTTLED` with
`throttle_delay_seconds`. An entity may be 256 KiB. When the server holds
`MAX_CHAINS` chains, a new chain's first request gets `TRANSIENT_ERROR`.

## Events

`GET /v2/events?cache_guid=<the device's cache GUID>` opens a Server-Sent Events
stream:

| Event | Data | What the client does |
|---|---|---|
| `ready` | `{}` | The stream is open. Fetch updates, since changes may have arrived while it was closed |
| `changed` | `{"types":[32904]}` | Fetch updates of these data types (IDs are `EntitySpecifics` field numbers) |
| `resync` | `{}` | The server may have lost events. Fetch updates of all data types |

A device is not told about its own commits; the server recognizes it by the
cache GUID of the commit. A comment line every 25 seconds keeps proxies from
closing the stream, and `retry: 5000` tells the client to reconnect after 5
seconds. Events are hints: a client that misses one still gets the change on its
next poll, which the server sets to once an hour.

## What the server stores

| Table | Contents |
|---|---|
| `chains` | Chain ID, birthday, keystore key, version counter, entity count |
| `entities` | Each entity as the client committed it, with its server ID and version |

**Encryption.** With a sync passphrase, Chromium encrypts every data type with a
key derived from the passphrase. The server then stores only ciphertext, plus
what Chromium leaves in the clear: entity IDs, versions, times and the bookmark
tree structure. Without a passphrase, Chromium encrypts only passwords and a few
other types, with the keystore key, which the server holds: the server operator
can read all of it.

**Deletions.** A deleted entity stays as a tombstone with its data removed, so
devices that were offline learn about the deletion. Tombstones are only removed
with their chain.
