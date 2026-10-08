-- The user-session helper reports its health as collector user_helper. The row already exists in a
-- database built from the current schema.sql, which the migrator also applies this file over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('user_helper', 'capture_core', ARRAY[]::text[], 'Helper process in each signed-in user session, which shows that user the agent''s notifications. Not a collection path; it reads nothing.')
ON CONFLICT (collector_code) DO NOTHING;
