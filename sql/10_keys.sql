CREATE TABLE _keys (
    op              CHAR NOT NULL,      -- 'A' for adding, 'R' for revoking
    pubkey_hash     VARCHAR NOT NULL,   -- in the format 'type:hex'
    pubkey          VARCHAR NOT NULL,   -- hex-encoded
    sigkey_hash     VARCHAR NOT NULL,   -- same format as pubkey_hash
    signature       VARCHAR NOT NULL,
    metadata        VARCHAR,
    PRIMARY KEY (pubkey_hash, sigkey_hash)
);
