-- Accounts (/v3): devices with keys of their own, the account key in
-- envelopes the server cannot open, and the chains the account syncs.
CREATE TABLE accounts (
    id         text PRIMARY KEY,
    kind       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- The account key, wrapped on the client; id is the lookup key the client
-- derives. One envelope per scheme and account.
CREATE TABLE envelopes (
    id            text PRIMARY KEY,
    account_id    text NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    scheme        text NOT NULL,
    salt          bytea NOT NULL,
    kdf           jsonb NOT NULL,
    verifier      bytea NOT NULL,
    verifier_salt bytea NOT NULL,
    wrapped       bytea NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, scheme)
);

-- A removed device keeps its row until retention removes it, so that its
-- key cannot join the account again.
CREATE TABLE devices (
    account_id   text NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    public_key   bytea NOT NULL,
    name         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz NOT NULL DEFAULT now(),
    removed_at   timestamptz,
    PRIMARY KEY (account_id, public_key)
);
CREATE INDEX devices_removed ON devices (removed_at) WHERE removed_at IS NOT NULL;

-- The chains an account claimed. A claim does not reference chains: it is
-- made before the chain exists and outlives a ClearServerData, so the chain
-- stays the account's when a device creates it again.
CREATE TABLE account_chains (
    chain_id   text PRIMARY KEY,
    account_id text NOT NULL REFERENCES accounts (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_chains_account ON account_chains (account_id);
