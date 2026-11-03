CREATE TABLE chains (
    id            text PRIMARY KEY,
    birthday      text NOT NULL,
    keystore_key  bytea NOT NULL,
    version       bigint NOT NULL DEFAULT 0,
    live_entities bigint NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE entities (
    chain_id   text NOT NULL REFERENCES chains (id) ON DELETE CASCADE,
    id         text NOT NULL,
    data_type  integer NOT NULL,
    version    bigint NOT NULL,
    deleted    boolean NOT NULL,
    server_tag text NOT NULL DEFAULT '',
    data       bytea NOT NULL,
    PRIMARY KEY (chain_id, id)
);

-- GetUpdates: a chain's entities of one data type after a version.
CREATE INDEX entities_changes ON entities (chain_id, data_type, version);
