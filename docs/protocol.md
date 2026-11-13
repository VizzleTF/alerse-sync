# How the server works

This page explains what the sync server expects from clients and what it keeps.
It is for people working on Alerse's sync client or running a server. The
server speaks only `/v3`: accounts with the 24 words and a password, a key per
device, and the chains of each account ([History](#history) lists what was
removed).

## Accounts and device tokens

An account is a user's set of devices and the chains they sync. Each device
signs its requests with an Ed25519 key of its own, so the server can list
devices and refuse a removed one. The server keeps the account key only in an
envelope it cannot open. `internal/account` implements it; the design is
ADR-0002 of Alerse. This server offers one scheme, `sk-v1`: the 24 words and a
password, no email.

**What the client derives** (scheme `sk-v1`). HKDF-SHA256 takes no salt and
gives 32 bytes; the password is UTF-8 in Unicode NFC.

```text
sk      = the 256 bits the 24 words encode
id      = hex(HKDF-SHA256(sk, info "alerse-sk-id"))      names the envelope
salt    = 32 random bytes, chosen by the client when it creates the account
pwkey   = scrypt(password, salt, N, r, p, 32 bytes)
master  = HKDF-SHA256(pwkey | HKDF-SHA256(sk, info "alerse-sk-mix"), info "alerse-sk-v1")
auth    = HKDF-SHA256(master, info "alerse-sk-auth")     sent to the server
wrap    = HKDF-SHA256(master, info "alerse-sk-wrap")     never leaves the device
wrapped = 0x01 | nonce (12) | AES-256-GCM(wrap, nonce, account key, aad "alerse-sk-v1" | id)
```

The KDF is scrypt with N = 2^17, r = 8, p = 1 by default, and the server refuses N below 2^15 or more than 1 GiB. Clients
refuse parameters below their own as well. Without the words nobody can name
the envelope or test a password offline; without the password the words open
nothing. The account key is 32 random bytes made on the first device; the
account's chains derive from it (Alerse ADR-0001 and ADR-0002).
`e2e/accounts_test.go` derives all of this as a client does.

**Device tokens.** Every authenticated `/v3` request carries
`Authorization: Bearer <token>`:

```text
token     = base64url, no padding (0x02 | account (32) | device public key (32) | issued at (8) | signature (64))
issued at = Unix time in milliseconds, 8 bytes big-endian
signature = Ed25519(device key, "alerse-sync-device-token-v1" | account | public key | issued at)
```

`account` is the account ID the server handed out, as 32 bytes. The server
accepts a token from 24 hours before to 5 minutes after its own clock and keeps
no session state, so a client can issue a new token for every request. On every
request it checks that the key is an active device of the account. A bad or
expired token gets HTTP 401, which makes Chromium ask for a new one.
`internal/auth` implements this. [Refusing abuse](#refusing-abuse) says what
the server remembers to skip the database.

**Requests.** Bodies are JSON; byte strings are standard base64. IDs are 64
lowercase hex digits; a device's ID is its public key in hex.

| Request | Auth | Body | Answer |
|---|---|---|---|
| `POST /v3/unlock/params` | none | `{"id"}` | `{"scheme","salt","kdf":{"name":"scrypt","n","r","p"}}` |
| `POST /v3/account` | none | `{"id","scheme","salt","kdf","auth","wrapped","device":{"public_key","name"}}` | 201 `{"account"}`; 409 if the words have an account |
| `POST /v3/unlock` | none | `{"id","auth","device":{"public_key","name"}}` | `{"account","wrapped"}`; 401 `wrong words or password`; 409 for a removed key or too many devices |
| `GET /v3/account` | device | | `{"account","kind","created"}` |
| `DELETE /v3/account` | device | `{"auth"}` | 204 |
| `GET /v3/devices` | device | | `{"devices":[{"id","name","created","last_seen","current"}]}` |
| `DELETE /v3/devices/{id}` | device | `{"auth"}`, or none for the device itself | 204; 404; 409 for the account's only device |
| `POST /v3/chains/{chain}/command/` | device | protocol | [The sync protocol](#the-sync-protocol) |
| `GET /v3/chains/{chain}/events` | device | | [Events](#events) |

- **Create** makes the account with its first device. **Unlock** adds the
  device that sends it, or renames it if it is in the account already. A
  device name is 1 to 64 characters without control characters.
- **Removing a device or the account** takes `auth` again, so a stolen device
  that is still signed in cannot lock its owner out. A device signs itself out
  without it, since it can only lose its own access, while another device is
  left; the last one goes only with the account. A removed key never joins the
  account again: the client makes a new key.
- **One installation, one key.** The browser keeps its device key for as long
  as it is installed and signs in again with the same key, which adds nothing.
  A key new to the account takes the words and the password, counts against
  `NEW_DEVICES_PER_ACCOUNT_PER_DAY` and `MAX_DEVICES_PER_ACCOUNT`, and shows in
  every device's list.
- **Deleting the account** removes its envelope, devices and chains with all
  their data, as ClearServerData does for one chain. The chains' event streams
  get `changed`, and the devices' next requests get 401.

**An account's chains.** A Space's sync URL is
`https://<server>/v3/chains/<chain>`; Chromium adds `/command/` and the events
client `/events`. `chain` is the chain ID: hex(SHA-256) of the chain's public
key, which derives from the account key. A chain becomes the
account's the first time one of its devices uses it, and stays the account's
after a ClearServerData. These get 403:

- a chain of another account;
- an existing chain of no account, so an account's chains always start empty;
- a chain over `MAX_CHAINS_PER_ACCOUNT`.

**No enumeration.** For an `id` without an account, params answers the
default KDF and a salt made up with HMAC-SHA256 under a server secret, and
unlock does the same Argon2id work before its 401. Create
answers 409 for an `id` in use, which tells nothing: only whoever holds the
words can derive the `id`.

**Rate limits.** Over a limit the answer is 429 with `Retry-After`:

| Limit | Counts |
|---|---|
| `NEW_ACCOUNTS_PER_IP_PER_DAY` (10) | Creates per client address |
| `UNLOCK_LOOKUPS_PER_IP_PER_HOUR` (120) | Params per client address |
| `UNLOCK_ATTEMPTS_PER_HOUR` (10) | Password checks per envelope: unlocks, and `auth` on removals. The right password counts too |
| `UNLOCK_ATTEMPTS_PER_IP_PER_HOUR` (30) | Password checks per client address |
| `ACCOUNT_CHANGES_PER_HOUR` (120) | Account and device requests per account |
| `NEW_DEVICES_PER_ACCOUNT_PER_DAY` (5) | Unlocks that add a key the account does not have |

`MAX_ACCOUNTS` answers 503 to a create when the server is full,
`MAX_DEVICES_PER_ACCOUNT` 409 to an unlock with a new key.

**What the server learns:** the envelope id, the client's salt and KDF
parameters, Argon2id of `auth` (19 MiB, 2 passes, a random salt of its own),
the wrapped account key, the devices' public keys and names, when each was
created and last seen (to 5 minutes), and which chains are one account. It
never learns the words, the password or the account key. A copy of the
database alone allows no password guessing: each guess needs the words too.

**Not built yet:** email accounts (`email-v1`) and their recovery code
(`rk-v1`), pairing a new device with a short code (`/v3/pair`), changing the
password or the keys. The event stream of a removed device stays open until it reconnects; it
carries only data type numbers.

## The sync protocol

`POST /v3/chains/{chain}/command/` takes a `ClientToServerMessage` and answers with a
`ClientToServerResponse`, the messages of Chromium's
`components/sync/protocol/sync.proto`. Request and response bodies may be
gzip-compressed. The server handles three requests:

| Request | What the server does |
|---|---|
| GetUpdates | Returns each requested data type's entities that changed after the client's progress token, oldest first, up to 500 entities or 16 MiB per response. `changes_remaining` tells the client to ask again. With `need_encryption_key` it adds the chain's keystore key |
| Commit | Stores the entities in one transaction and answers each one: `SUCCESS` with its ID and version, `CONFLICT` if the client's version is out of date, `OVER_QUOTA`, `TRANSIENT_ERROR` or `INVALID_MESSAGE` (see Limits) |
| ClearServerData | Deletes the chain. Its other devices get `NOT_MY_BIRTHDAY`, turn sync off and clear their sync data |

**Versions.** Each chain has a counter. Every committed entity gets the next
value as its version, under a lock on the chain, so versions only grow. A
progress token is the highest version a client has of a data type. Tokens are
opaque to clients:

```text
incremental = 0x01 | version (8 bytes, big-endian)
snapshot    = 0x02 | version reached (8) | chain version when the download began (8)
```

A data type whose version is not past the client's token costs no database
read, and its new token is the chain's version, so the next request skips it
as well.

**First sync.** A client without a token gets the live entities of the type,
without tombstones: it has nothing to delete. While the download pages, the
client holds a snapshot token; entities that change during the download get
newer versions and follow in the same sync cycle, deletions included.

**Removed tombstones.** The server removes tombstones after `TOMBSTONE_TTL`
(see [What the server stores](#what-the-server-stores)). A client whose token is
older than a removed tombstone may have missed that deletion, so it downloads
the type's live entities again, like a first sync. The first response of that
download carries `gc_directive { version_watermark }` in the type's progress
marker, which makes Chromium drop the entities it does not receive. Devices
that sync more often than the TTL never see this.

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

**History.** Chromium's history bridge forgets a visit's version once it is
committed, so a later change to the visit arrives with version 0. Such a commit
replaces the stored visit instead of conflicting, as on Google's server. Other
types conflict, except a deleted entity created again from version 0.

**Limits.** Commits are answered per entity:

| Limit | Answer |
|---|---|
| An entity over 256 KiB | `INVALID_MESSAGE` |
| `MAX_BYTES_PER_CHAIN`, counting tombstones | `OVER_QUOTA`, after history made what room it can |
| `MAX_ENTITIES` live entities of the types not limited below: bookmarks, passwords, settings and so on | `OVER_QUOTA` |
| `MAX_SESSION_ENTITIES` open tabs and windows | `OVER_QUOTA` |
| `MAX_HISTORY_ENTITIES` or `MAX_HISTORY_BYTES` of history and history delete directives | `SUCCESS`: the oldest visits are dropped, a hundredth more than needed. Clients expire history on their own, so nothing tells them |
| `REQUIRE_CUSTOM_PASSPHRASE`, data of a type Chromium can encrypt while the chain's Nigori node is not in `CUSTOM_PASSPHRASE`, or such data unencrypted | `TRANSIENT_ERROR`: Chromium keeps the entity and commits it again later. Deletions, the Nigori node and never-encrypted types such as device info pass |

**Commit delays.** Chromium waits before it commits a local change: the wait
starts at the first change, and everything that changed meanwhile goes in one
commit. A device with no changes sends nothing. Every response sets the wait
(`client_command.custom_nudge_delays`, which Chromium applies as is):

| Data | Wait | Chromium's own |
|---|---|---|
| Open tabs and windows | `SESSIONS_NUDGE_DELAY`, 15 s | 15 s |
| Every other type | `NUDGE_DELAY`, 60 s | 2 s for bookmarks and settings, less for passwords |
| Autofill, user events, encryption keys, device info, messages to other devices | Chromium's | up to 8 h; nothing for messages |

The wait is a request to a well-behaved client. The server enforces its own
limit on commits, `COMMITS_PER_CHAIN_PER_HOUR` below, whatever a client does.

Deletions are always accepted. The limits on requests:

| Limit | Answer |
|---|---|
| `RATE_LIMIT_PER_MINUTE` per chain, `RATE_LIMIT_PER_IP_PER_MINUTE` per client address | `THROTTLED` with `throttle_delay_seconds` |
| `COMMITS_PER_CHAIN_PER_HOUR` (360, one per 10 s), of which `COMMIT_BURST` (120) at once for a first upload | `THROTTLED` with `throttle_delay_seconds` |
| `NEW_CHAINS_PER_IP_PER_DAY`, on the request that would create a chain | `THROTTLED` until the address may create one |
| `MAX_CHAINS` | `TRANSIENT_ERROR` on a new chain's first request |
| No free slot of `MAX_CONCURRENT_REQUESTS` within 10 seconds | HTTP 503 with `Retry-After` |
| A body that takes longer than `BODY_READ_TIMEOUT` | HTTP 408. Bodies are read before a request takes a slot |

A client address is the connection's, or the one `TRUSTED_PROXY` proxies name
in `CLIENT_IP_HEADER`. IPv6 clients count by their /64.

## Refusing abuse

A request passes these checks in order, each cheaper than the next, so that
what a broken or hostile client sends costs the server as little as possible:

| Step | Check | Costs | Refused with |
|---|---|---|---|
| 1 | The address is shut out | a map lookup | 429 with `Retry-After` |
| 2 | `RATE_LIMIT_PER_IP_PER_MINUTE` | a map lookup | `THROTTLED`, or 429 |
| 3 | The token's format, age and Ed25519 signature | about 50 µs | 401 |
| 4 | `DEVICE_REQUESTS_PER_MINUTE` per device key, `DEVICE_REQUEST_BURST` at once | a map lookup | 429 |
| 5 | The key is an active device of the account | memory; else one query | 401; 503 over `UNKNOWN_ACCOUNT_LOOKUPS_PER_SECOND` |
| 6 | The chain is the account's | memory; else one query | 403 |
| 7 | `RATE_LIMIT_PER_MINUTE` per chain | a map lookup | `THROTTLED` |
| 8 | A declared body over `MAX_REQUEST_BYTES` | none: the body is not read | 413 |
| 9 | The body, read within `BODY_READ_TIMEOUT`, decompressed, parsed | the body | 400, 408, 413 |
| 10 | `COMMITS_PER_CHAIN_PER_HOUR` for a commit | a map lookup | `THROTTLED` |

**Shutting out an address.** `BAN_AFTER_FAILURES` (30) refused requests from
one address within `BAN_WINDOW` (10 minutes) shut it out for `BAN_DURATION`
(15 minutes): its requests then get 429 before step 2. Refused means a 400,
401, 403, 404, 405, 408, 413 or 429 answer, or `THROTTLED`: what a client that
follows the protocol and its limits does not get often. A device whose chain
does not exist yet is not refused when it asks for events. The address is
logged once when it is shut out.

**What the server remembers**, on each instance:

- Device keys it found active, for 30 seconds: a removal on another instance
  takes effect within that, one on the same instance at once.
- Keys an account does not have, for 10 minutes, so a refused key is refused
  again without a query.
- Accounts with an active device, for 24 hours. An account ID comes from the
  account's words and is never published, so a token naming an account the
  instance has not seen is almost always made up: all such lookups together get
  `UNKNOWN_ACCOUNT_LOOKUPS_PER_SECOND` (20, five times that at once). Under
  attack, a real device the instance has not seen since its start waits a few
  seconds at most.
- Chain owners, for 10 minutes. An owner changes only when the chain is
  deleted, and only the owner can derive a chain ID.

In a test on one machine, made-up tokens from thousands of addresses reached
the database 199 times in 5 seconds instead of 12,242, and a shut out address
got 85,000 refusals a second.

## Events

`GET /v3/chains/{chain}/events?cache_guid=<the device's cache GUID>` opens a
Server-Sent Events stream:

| Event | Data | What the client does |
|---|---|---|
| `ready` | `{}` | The stream is open. Fetch updates, since changes may have arrived while it was closed |
| `changed` | `{"types":[32904]}` | Fetch updates of these data types (IDs are `EntitySpecifics` field numbers) |
| `resync` | `{}` | The server may have lost events. Fetch updates of all data types |

A device is not told about its own commits; the server recognizes it by the
cache GUID of the commit. A stream of a chain that does not exist yet gets HTTP
404: the device's first GetUpdates creates the chain, and the device connects
again. Streams over `MAX_EVENT_STREAMS` per chain or `MAX_EVENT_STREAMS_PER_IP`
per address get 429; at shutdown, streams end and new ones get 503. A comment line every 25 seconds keeps proxies from
closing the stream, and `retry: 5000` tells the client to reconnect after 5
seconds. Events are hints: a client that misses one still gets the change on its
next poll, which the server sets to once an hour.

## What the server stores

| Table | Contents |
|---|---|
| `chains` | Chain ID, birthday, keystore key, version counter, entity count, size, whether the Nigori node says custom passphrase, when a device last talked to the server |
| `chain_types` | Per chain and data type: highest version, highest removed tombstone, live entities, tombstones, size |
| `chain_count` | The number of chains, kept by a trigger |
| `entities` | Each entity as the client committed it, with its server ID, version and the time it was written |
| `server_secrets` | Random values shared by all instances, such as the key for made-up salts |
| `accounts` | Account ID (random), kind (`sk`), when it was created |
| `envelopes` | Per account and scheme: the envelope id, the client's salt and KDF parameters, an Argon2id hash of the authentication key, the account key encrypted on the device |
| `devices` | Per account: each device's public key and name, when it was created, last seen and removed |
| `account_chains` | Which account claimed a chain. It outlives the chain, so a chain cleared and created again stays the account's |

**Encryption.** With a sync passphrase, Chromium encrypts every data type with a
key derived from the passphrase. The server then stores only ciphertext, plus
what Chromium leaves in the clear: entity IDs, versions, times and the bookmark
tree structure. Without a passphrase, Chromium encrypts only passwords and a few
other types, with the keystore key, which the server holds: the server operator
can read all of it. `REQUIRE_CUSTOM_PASSPHRASE` makes the server refuse such
data.

**Deletions.** A deleted entity stays as a tombstone with its data removed, so
devices that were offline learn about the deletion, for `TOMBSTONE_TTL` (60
days). After that the server removes it, and a device that has not synced
since downloads the data type again, as described under
[Removed tombstones](#the-sync-protocol).

**Retention.** A job removes, every `RETENTION_INTERVAL`:

| What | After | Why |
|---|---|---|
| Tombstones | `TOMBSTONE_TTL`, 60 days | See above |
| History and history delete directives | `HISTORY_TTL`, 90 days since they were last written | Chromium keeps 90 days of history and sends nothing when a visit expires: Google's server expires history itself, and so does this one. A directive outlives the history it deletes |
| Chains no device committed to | `EMPTY_CHAIN_TTL`, 7 days | Every Chromium device commits its device info right away; a chain with only its permanent entities was never used |
| Chains no device talked to | `INACTIVE_CHAIN_TTL`, off by default | Abandoned chains on a public server |
| Records of removed devices | `REMOVED_DEVICE_TTL`, 90 days | Until then the removed key cannot join the account again |

Removing data takes the chain's lock, one chain at a time, so it never races a
commit. With several instances, one runs the job at a time.

## History

Releases up to 0.1.5 also served chains without accounts: every device of a
chain derived its key from the 24 words alone (`/v2/command/`, `/v2/events`),
with an optional login name and password (`/v2/login`, `pw-v1`) and pairing by
short code (`/v2/pair`, `pair-v1`). 0.2.0 removed all of it: old clients get
404, and the migration to 0.2.0 drops the sign-ins, the pairing slots and every
chain no account owns. Sync starts over on `/v3`.
