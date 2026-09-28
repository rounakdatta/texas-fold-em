-- +goose Up
-- What a web search found about a place the payee picker couldn't place:
-- its usual name, what it is, and its branches, per place and where the
-- payment was made (at home, in a city, abroad). A lookup is seconds long
-- and places rarely move, so each is kept a month (a place not found, three
-- days) and the next card that names the place has its branches at once.
CREATE TABLE place_lookups (
    lookup_key   TEXT PRIMARY KEY, -- the place's name as compared, "|", where
    name         TEXT NOT NULL,    -- the name that was looked up
    facts_json   TEXT NOT NULL,    -- classifier.PlaceFacts
    looked_up_at TEXT NOT NULL     -- RFC 3339
);

-- +goose Down
DROP TABLE place_lookups;
