-- Sizes and counts per data type, which quotas, GetUpdates and the removal
-- of old tombstones need without scanning entities.

ALTER TABLE chains
    ADD COLUMN bytes_used        bigint NOT NULL DEFAULT 0,
    ADD COLUMN custom_passphrase boolean NOT NULL DEFAULT false,
    ADD COLUMN last_seen_at      timestamptz NOT NULL DEFAULT now();

ALTER TABLE entities
    ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();

CREATE TABLE chain_types (
    chain_id       text NOT NULL REFERENCES chains (id) ON DELETE CASCADE,
    data_type      integer NOT NULL,
    -- The highest version an entity of the type has had.
    version        bigint NOT NULL DEFAULT 0,
    -- The highest version of a removed tombstone of the type.
    purged_version bigint NOT NULL DEFAULT 0,
    entities       bigint NOT NULL DEFAULT 0,
    tombstones     bigint NOT NULL DEFAULT 0,
    bytes          bigint NOT NULL DEFAULT 0,
    PRIMARY KEY (chain_id, data_type)
);

INSERT INTO chain_types (chain_id, data_type, version, entities, tombstones, bytes)
SELECT chain_id, data_type, max(version),
       count(*) FILTER (WHERE NOT deleted), count(*) FILTER (WHERE deleted),
       sum(octet_length(data))
FROM entities
GROUP BY chain_id, data_type;

UPDATE chains SET bytes_used = t.bytes
FROM (SELECT chain_id, sum(bytes) AS bytes FROM chain_types GROUP BY chain_id) t
WHERE chains.id = t.chain_id;

-- The number of chains, kept by a trigger so that creating a chain can check
-- MAX_CHAINS without counting, and atomically: the row stays locked until
-- the creating transaction ends.
CREATE TABLE chain_count (
    id boolean PRIMARY KEY DEFAULT true CHECK (id),
    n  bigint NOT NULL
);
INSERT INTO chain_count (n) SELECT count(*) FROM chains;

-- A statement that changes no row leaves the row unlocked.
CREATE FUNCTION chain_count_insert() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k bigint;
BEGIN
    SELECT count(*) INTO k FROM inserted;
    IF k > 0 THEN
        UPDATE chain_count SET n = n + k;
    END IF;
    RETURN NULL;
END $$;

CREATE FUNCTION chain_count_delete() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE k bigint;
BEGIN
    SELECT count(*) INTO k FROM deleted;
    IF k > 0 THEN
        UPDATE chain_count SET n = n - k;
    END IF;
    RETURN NULL;
END $$;

CREATE TRIGGER chain_count_insert AFTER INSERT ON chains
    REFERENCING NEW TABLE AS inserted FOR EACH STATEMENT EXECUTE FUNCTION chain_count_insert();
CREATE TRIGGER chain_count_delete AFTER DELETE ON chains
    REFERENCING OLD TABLE AS deleted FOR EACH STATEMENT EXECUTE FUNCTION chain_count_delete();

-- Removal of old tombstones and of expired history.
CREATE INDEX entities_tombstones ON entities (updated_at) WHERE deleted;
CREATE INDEX entities_expiring ON entities (updated_at) WHERE data_type IN (963985, 150251);
-- A first sync leaves tombstones out.
CREATE INDEX entities_live ON entities (chain_id, data_type, version) WHERE NOT deleted;
-- Removal of chains nobody uses.
CREATE INDEX chains_empty ON chains (created_at) WHERE version <= 5;
CREATE INDEX chains_last_seen ON chains (last_seen_at);
