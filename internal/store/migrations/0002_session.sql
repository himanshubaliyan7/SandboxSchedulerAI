-- 0002_session: caller-supplied metadata of a session, set at claim time
-- by the API layer (string keys and values) and used to filter listings.
ALTER TABLE sandboxes ADD COLUMN metadata jsonb;
