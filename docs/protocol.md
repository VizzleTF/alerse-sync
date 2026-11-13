# How the server works

This page explains what the sync server expects from clients and what it keeps.
It is for people working on Alerse's (formerly Arcomium's) sync client or running a server.

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

**Commit delays.** Chromium waits before it commits a local change, to merge a
burst of them: 2 seconds for bookmarks, 15 for open tabs. Every response tells
clients to wait `SESSIONS_NUDGE_DELAY` (3 seconds) for open tabs instead
(`client_command.custom_nudge_delays`, which Chromium applies as is), so a tab
reaches the other devices about 3 to 4 seconds after it opened.

Deletions are always accepted. The limits on requests:

| Limit | Answer |
|---|---|
| `RATE_LIMIT_PER_MINUTE` per chain, `RATE_LIMIT_PER_IP_PER_MINUTE` per client address | `THROTTLED` with `throttle_delay_seconds` |
| `NEW_CHAINS_PER_IP_PER_DAY`, on the request that would create a chain | `THROTTLED` until the address may create one |
| `MAX_CHAINS` | `TRANSIENT_ERROR` on a new chain's first request |
| No free slot of `MAX_CONCURRENT_REQUESTS` within 10 seconds | HTTP 503 with `Retry-After` |
| A body that takes longer than `BODY_READ_TIMEOUT` | HTTP 408. Bodies are read before a request takes a slot |

A client address is the connection's, or the one `TRUSTED_PROXY` proxies name
in `CLIENT_IP_HEADER`. IPv6 clients count by their /64.

## Sign-in with a login name and password

A chain may also have one sign-in, so that a new device can join with a login
name (an email address or any other name) and a password instead of the 24
words. It is optional, and the words keep working. The server stays
zero-knowledge: it never sees the login name, the password or the chain's
secret, only values derived from them on the device. `internal/login`
implements the server side.

**What the client derives** (scheme `pw-v1`):

```text
login  = the login name, without surrounding spaces, ASCII letters in lower case
id     = hex(SHA-256("arcomium-sync-login-id-v1" | 0x00 | login))
salt   = 32 random bytes, chosen by the client when it sets the sign-in
master = scrypt(password, "arcomium-sync-login-v1" | 0x00 | id | salt, N, r, p, 32 bytes)
auth   = HKDF-SHA256(master, info "arcomium-sync-login-auth"), 32 bytes
enc    = HKDF-SHA256(master, info "arcomium-sync-login-enc"), 32 bytes
secret = 0x01 | nonce (12) | AES-256-GCM(enc, nonce, chain secret, aad "arcomium-sync-login-v1" | id)
```

scrypt with N = 2^17, r = 8, p = 1 (128 MiB) is the default. Argon2id would
be the first choice, but Chromium's tree has no Argon2, and scrypt is in its
BoringSSL. The server sends `auth` to prove the password and never gets
`enc`, which alone opens the secret.

**Requests.** Bodies are JSON; byte strings are standard base64.

| Request | Auth | Body | Answer |
|---|---|---|---|
| `POST /v2/login/params` | none | `{"id"}` | `{"scheme","salt","kdf":{"name":"scrypt","n","r","p"}}` |
| `POST /v2/login/unlock` | none | `{"id","auth"}` | `{"secret"}`, or 401 `wrong login or password` |
| `GET /v2/login` | token | | `{"id","scheme","kdf","updated"}`, or 404 when the chain has none |
| `PUT /v2/login` | token | `{"id","scheme","salt","kdf","auth","secret"}` | 204; 409 when another chain has the login |
| `DELETE /v2/login` | token | | 204 |

`PUT` sets the chain's sign-in, replacing its earlier one: a new password or a
new login name. It only wraps the same chain secret again; the chain's data
stays as it is. If no device has synced yet, `PUT` creates the chain, under the
limits of a first GetUpdates. A chain's sign-in goes when the chain goes:
ClearServerData and the retention job remove it too.

**What the server stores** (`logins`): the id, the chain, the scheme, the
client's salt and KDF parameters, the wrapped secret, and
Argon2id(`auth`, a random salt of its own; 19 MiB, 2 passes) rather than
`auth` or a fast hash of it, so a copy of the database alone does not give a
cheap way to test passwords. Unlock compares in constant time.

**No enumeration.** For an id without a sign-in, params answers with the
default KDF and a salt made up as HMAC-SHA256 of the id under a random server
secret (kept in `server_secrets`, so every instance and restart answers the
same), and unlock does the same Argon2id work before answering 401. One gap
stays: `PUT` with a login another chain has answers 409, so whoever runs a
chain can learn that a login name is in use, at `LOGIN_CHANGES_PER_HOUR`.

**Rate limits.** Unlock attempts are limited per id (`LOGIN_ATTEMPTS_PER_HOUR`,
10) and per client address (`LOGIN_ATTEMPTS_PER_IP_PER_HOUR`, 30), params per
address (`LOGIN_LOOKUPS_PER_IP_PER_HOUR`, 120), and changes per chain
(`LOGIN_CHANGES_PER_HOUR`, 30). Over a limit the answer is 429 with
`Retry-After`. The per-id limit holds even for the right password, so someone
who guesses at a login name can lock its owner out for an hour; the owner
still has the words. Limits are per instance, like the others.

**What it does not protect against.** The operator of the server, or anyone
with its database, can test passwords offline: each guess costs one scrypt of
128 MiB and an Argon2id. A long password is the only defence, so clients ask
for at least 15 characters (NIST SP 800-63B-4) and refuse the most common
ones. A server that sends weak KDF parameters could make a joining client
send an `auth` that is cheap to attack, so clients refuse parameters below
their own minimum. The sync code itself, 256 random bits, cannot be guessed.

Login names are not verified: nobody checks that an email address belongs to
whoever uses it, and the first chain to set a login name has it. On a public
server, email addresses should be verified before they can be claimed; that
comes with mail, which does not exist yet. Mailing a chain's sync code to the
user was rejected: mail is a weak channel, and sending it from the server would
hand the code to the server. Clients offer a printable recovery sheet instead.

Later schemes, such as OPAQUE (RFC 9807), which would also hide `auth` from
the server, can be added next to `pw-v1`: every record names its scheme. A
second factor (TOTP) is planned for a public server, not built.

## Pairing a new device

A device of a chain can hand the chain's secret to a new device through the
server, with a short one-time code that the user reads on one screen and types
on the other. The server only relays; `internal/pairing` implements it.

**What the devices do** (scheme `pair-v1`):

```text
code    = 12 characters of Crockford base32 (0-9, A-Z without I, L, O, U),
          shown XXXX-XXXX-XXXX; O reads as 0, I and L as 1
slot    = hex(SHA-256("arcomium-pair-slot-v1" | 0x00 | first 4 characters))
SPAKE2  = BoringSSL's SPAKE2 (Curve25519), the existing device as Alice with
          name "arcomium-pair-existing", the new one as Bob with name
          "arcomium-pair-new", password
          "arcomium-pair-v1" | 0x00 | slot | 0x00 | last 8 characters (40 bits)
keys    = HKDF-SHA256(SPAKE2 key, info "arcomium-pair-confirm", "arcomium-pair-box",
          "arcomium-pair-verify"), 32 bytes each
confirm = HMAC-SHA256(confirm key, "arcomium-pair-new-device" | 0x00 | device name)
number  = first 4 bytes of the verify key, big-endian, mod 10000: shown on both
box     = 0x01 | nonce (12) | AES-256-GCM(box key, chain secret, aad "arcomium-pair-v1" | slot)
```

1. The existing device (A) opens a slot with its SPAKE2 message.
2. The new device (B) reads the slot, finishes SPAKE2, and joins with its own
   message, its name and `confirm`.
3. A finishes SPAKE2 and checks `confirm`. A wrong code fails here; A closes the
   slot. Otherwise both show `number`, and A asks the user to allow B by name.
4. On Allow, A approves with `box`; B reads it and opens it.

**Requests.** Bodies are JSON; byte strings are standard base64.

| Request | Auth | Body | Answer |
|---|---|---|---|
| `POST /v2/pair` | token | `{"slot","msg"}` | 204; 409 if the slot is in use |
| `GET /v2/pair/{slot}` | none | | `{"msg","joined","bmsg","name","confirm","box","expires"}`, or 404 |
| `POST /v2/pair/{slot}/join` | none | `{"msg","name","confirm"}` | 204; 409 if someone joined already |
| `POST /v2/pair/{slot}/approve` | token, the slot's chain | `{"box"}` | 204; 409 before a join or after an approval |
| `DELETE /v2/pair/{slot}` | token, the slot's chain | | 204 |

A slot lives 5 minutes (`expires` counts the seconds left), takes one join and
one approval, and goes with its chain. Opening is limited per chain
(`PAIR_CODES_PER_HOUR`, 20), joins and reads per client address
(`PAIR_JOINS_PER_IP_PER_HOUR`, 30; `PAIR_READS_PER_IP_PER_HOUR`, 1200; devices
poll every 2 seconds).

**What the server learns:** the slot id, the two SPAKE2 messages, the new
device's name, `confirm` and the encrypted secret. SPAKE2 messages allow no
offline guessing of the password, so the server, or anyone who reads the slot,
can only try one code online by joining first; that fails `confirm`, A closes
the slot, and the user makes a new code. A relay that answered both sides with
its own messages would need the code too, and the number on both screens shows
it.

## Events

`GET /v2/events?cache_guid=<the device's cache GUID>` opens a Server-Sent Events
stream:

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
| `logins` | A chain's optional sign-in: a hash of the login name, the client's salt and KDF parameters, an Argon2id hash of the authentication key, the chain secret encrypted on the device |
| `server_secrets` | Random values shared by all instances, such as the key for made-up salts |
| `pair_slots` | Pairings in progress, for 5 minutes: the slot id, the chain, the SPAKE2 messages, the new device's name and confirmation, the encrypted secret |

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

Removing data takes the chain's lock, one chain at a time, so it never races a
commit. With several instances, one runs the job at a time.
