-- +goose Up
-- The time zone of a town a trip's payees name, as a model placed it:
-- "Maple Hollow", beside New York's places, is America/New_York. fold's own tables know the
-- big cities; this keeps the towns, per currency (a "Paris" paid in dollars
-- is Texas's), so each is asked about once. An empty zone is a place it
-- couldn't be sure of — asked again after a week.
CREATE TABLE place_zones (
    place_key   TEXT NOT NULL, -- the place as compared: "maple hollow"
    currency    TEXT NOT NULL,
    zone        TEXT NOT NULL, -- IANA, or '' for unsure
    resolved_at TEXT NOT NULL, -- RFC 3339
    PRIMARY KEY (place_key, currency)
);

-- +goose Down
DROP TABLE place_zones;
