-- The browser extension derives a tool's fingerprint from the destination host alone, versioned
-- as tf2. The tf1 rows, one per documented request shape, matched no real request and go; the tf2
-- rows name one fingerprint per host. Sanction decisions and overrides are per tool, so none is
-- lost. Aggregates written under tf1 fingerprints keep them and read as unrecognised tools until
-- they age out of the window.
--
-- The migrator also applies this file over a database built from the current schema.sql, so every
-- statement converges on that schema.

DELETE FROM ref.tool_catalogue
 WHERE signal_kind = 'extension' AND tool_fingerprint LIKE 'tf1:%';

INSERT INTO ref.tool_catalogue (tool_fingerprint, display_name, vendor, signal_kind, evidence, app_key) VALUES
  ('tf2:de4s6yclxfhnumvdm3ubzgd5jveiwkzapelxdcj2qdzd72qrridq', 'ChatGPT (web)', 'openai', 'extension', '{"host":"chatgpt.com"}', 'chatgpt_web'),
  ('tf2:mespeqbrgr4vb7rqmmrubxuglidtpu3e5xpjd7dwqkm2evicbwla', 'ChatGPT (web)', 'openai', 'extension', '{"host":"chat.openai.com"}', 'chatgpt_web'),
  ('tf2:6lwajg7v6mwycbn43eszknx6p3sqfegfsk6tj2p4yqovdd32mnfq', 'Gemini (web)', 'google', 'extension', '{"host":"gemini.google.com"}', 'gemini_web'),
  ('tf2:vjlb4hsqpngtmkhd4gwtgmcorqo26q2ezy7vp4erdqysbawzdx2q', 'Perplexity (web)', 'perplexity', 'extension', '{"host":"www.perplexity.ai"}', 'perplexity_web')
ON CONFLICT (tool_fingerprint) DO NOTHING;
