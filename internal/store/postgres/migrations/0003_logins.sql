-- Optional sign-in of a chain with a login name and password. The server
-- stores the client's wrapped chain secret and a slow hash of its
-- authentication key, under a hash of the login name.
CREATE TABLE logins (
    id            text PRIMARY KEY,
    chain_id      text NOT NULL UNIQUE REFERENCES chains (id) ON DELETE CASCADE,
    scheme        text NOT NULL,
    salt          bytea NOT NULL,
    kdf           jsonb NOT NULL,
    verifier      bytea NOT NULL,
    verifier_salt bytea NOT NULL,
    secret        bytea NOT NULL,
    updated_at    timestamptz NOT NULL DEFAULT now()
);

-- Random values of the server that its instances share, such as the key that
-- makes up salts for login names that have no login.
CREATE TABLE server_secrets (
    name  text PRIMARY KEY,
    value bytea NOT NULL
);
