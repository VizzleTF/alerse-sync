-- The legacy scheme (/v2) is gone: sign-ins with a login name, pairing slots
-- and the chains no account owns. Sync started over on /v3.
DROP TABLE IF EXISTS logins;
DROP TABLE IF EXISTS pair_slots;
DELETE FROM chains WHERE id NOT IN (SELECT chain_id FROM account_chains);
