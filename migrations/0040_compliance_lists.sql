-- The restrictive lists screening reads (FR-42, ADR-0018): the SDN list of OFAC, the consolidated list of the UN
-- Security Council and the PEPs of Decreto 830 de 2021, as their owners publish them. They are public reference
-- data, not records of the entity: a refresh replaces a list whole, and its row says which version it is and when
-- it was last refreshed, which screening checks before trusting it.

-- +goose Up
CREATE TABLE compliance.lists (
    list         text PRIMARY KEY CHECK (list IN ('ofac_sdn', 'un_consolidated', 'pep_co')),
    version      text NOT NULL CHECK (char_length(version) BETWEEN 1 AND 64),
    entries      integer NOT NULL CHECK (entries > 0),
    refreshed_at timestamptz NOT NULL DEFAULT clock_timestamp()
);

CREATE TABLE compliance.list_entries (
    list      text NOT NULL REFERENCES compliance.lists,
    entry_id  text NOT NULL CHECK (char_length(entry_id) BETWEEN 1 AND 64),
    names     text[] NOT NULL CHECK (cardinality(names) > 0),
    documents text[] NOT NULL,
    PRIMARY KEY (list, entry_id)
);

GRANT SELECT, INSERT, UPDATE (version, entries, refreshed_at) ON compliance.lists TO aluna_app;
GRANT SELECT, INSERT, DELETE ON compliance.list_entries TO aluna_app;

-- +goose Down
DROP TABLE compliance.list_entries;
DROP TABLE compliance.lists;
