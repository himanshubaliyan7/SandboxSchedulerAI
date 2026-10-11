-- 0004_template_egress: a template's default egress allowlist. A sandbox
-- created without a network policy of its own gets these entries (when
-- the template's network mode is allowlist); a create that names a policy
-- replaces them. They are applied at claim time, not baked into a warm
-- sandbox, so changing them does not bump the template version.
ALTER TABLE templates ADD COLUMN allow_domains text[] NOT NULL DEFAULT '{}';
ALTER TABLE templates ADD COLUMN allow_cidrs   text[] NOT NULL DEFAULT '{}';
