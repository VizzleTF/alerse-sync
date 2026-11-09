-- Device pairing: a short-lived relay of SPAKE2 messages and of the chain
-- secret encrypted with the key the two devices agree on.
CREATE TABLE pair_slots (
    id         text PRIMARY KEY,
    chain_id   text NOT NULL REFERENCES chains (id) ON DELETE CASCADE,
    a_msg      bytea NOT NULL,
    b_msg      bytea,
    b_name     text,
    b_confirm  bytea,
    box        bytea,
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX pair_slots_created ON pair_slots (created_at);
