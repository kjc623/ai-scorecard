-- The installed-app scanner reports its health as collector inventory_scanner. The row already
-- exists in a database built from the current schema.sql, which the migrator also applies this file
-- over.
INSERT INTO ref.collector (collector_code, component, modes_supported, description) VALUES
  ('inventory_scanner', 'capture_core', ARRAY['m0','m1','m2','m3'], 'Scheduled scan of the installed applications (uninstall entries and AppX/MSIX packages), matched against the app catalog. Establishes that a tool is present; reads no content.')
ON CONFLICT (collector_code) DO NOTHING;
