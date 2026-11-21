-- User profile storage for bionicpro-auth.
-- Safe to execute more than once. The application uses an UPSERT keyed by
-- keycloak_user_id, so repeated logins update the existing row.

CREATE TABLE IF NOT EXISTS user_profiles (
    keycloak_user_id TEXT PRIMARY KEY,
    username         TEXT,
    identity_provider TEXT,
    email            TEXT,
    display_name     TEXT,
    first_name       TEXT,
    last_name        TEXT,
    yandex_user_id   TEXT,
    profile_data     JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_profiles_email
    ON user_profiles (email);

CREATE INDEX IF NOT EXISTS idx_user_profiles_identity_provider
    ON user_profiles (identity_provider);


