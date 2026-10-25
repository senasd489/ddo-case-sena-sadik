-- 0001: temel ayarlar. Asıl alan şeması sonraki migrasyonlarda.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE app_info (
    key   text PRIMARY KEY,
    value text NOT NULL
);
INSERT INTO app_info(key, value) VALUES ('name', 'EnerjiHat');
