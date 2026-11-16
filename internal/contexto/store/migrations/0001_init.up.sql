-- Esquema inicial de la plataforma de contexto (fases 0-1).
-- Todas las tablas de negocio llevan tenant_id y política RLS, como en Vigía.

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE TABLE tenant (
    id         uuid PRIMARY KEY,
    name       text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Elementos de configuración. id = hash estable de la clave natural (clase|clave...).
CREATE TABLE ci (
    tenant_id       uuid        NOT NULL REFERENCES tenant(id),
    id              text        NOT NULL,
    natural_key     text        NOT NULL,
    class           text        NOT NULL,
    technology      text        NOT NULL DEFAULT '',
    name            text        NOT NULL,
    attrs           jsonb       NOT NULL DEFAULT '{}'::jsonb,
    manual_attrs    jsonb       NOT NULL DEFAULT '{}'::jsonb,  -- prevalecen sobre attrs; el descubrimiento no los toca
    sources         text[]      NOT NULL DEFAULT '{}',
    jobs            text[]      NOT NULL DEFAULT '{}',
    sites           text[]      NOT NULL DEFAULT '{}',
    ssii            text,                                       -- valor del atributo ssii_attr (COD_SSII)
    lifecycle       text        NOT NULL DEFAULT 'active'
                    CHECK (lifecycle IN ('active', 'nodata', 'stale', 'retired')),
    lifecycle_since timestamptz NOT NULL DEFAULT now(),
    first_seen      timestamptz NOT NULL DEFAULT now(),
    last_seen       timestamptz NOT NULL DEFAULT now(),
    content_hash    text        NOT NULL DEFAULT '',
    PRIMARY KEY (tenant_id, id)
);
CREATE INDEX ci_class_idx      ON ci (tenant_id, class);
CREATE INDEX ci_lifecycle_idx  ON ci (tenant_id, lifecycle);
CREATE INDEX ci_ssii_idx       ON ci (tenant_id, ssii) WHERE ssii IS NOT NULL;
CREATE INDEX ci_name_trgm_idx  ON ci USING gin (lower(name) gin_trgm_ops);
CREATE INDEX ci_attrs_idx      ON ci USING gin (attrs jsonb_path_ops);

-- Historial de cambios de atributos (tipo SCD-2). attr '_name' = cambio de nombre.
CREATE TABLE ci_attr_history (
    tenant_id  uuid        NOT NULL,
    ci_id      text        NOT NULL,
    changed_at timestamptz NOT NULL,
    attr       text        NOT NULL,
    old_value  text,
    new_value  text,
    source     text        NOT NULL DEFAULT 'discovery'
);
CREATE INDEX ci_attr_history_idx ON ci_attr_history (tenant_id, ci_id, changed_at DESC);

-- Relaciones con validez temporal: valid_to NULL = vigente.
CREATE TABLE relation (
    id         bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id  uuid        NOT NULL,
    src        text        NOT NULL,
    rel        text        NOT NULL,
    dst        text        NOT NULL,
    method     text        NOT NULL DEFAULT 'métricas',
    origin     text        NOT NULL DEFAULT 'discovered' CHECK (origin IN ('discovered', 'manual', 'inferred')),
    confidence real        NOT NULL DEFAULT 1,
    attrs      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    valid_from timestamptz NOT NULL DEFAULT now(),
    valid_to   timestamptz
);
CREATE UNIQUE INDEX relation_active_uq ON relation (tenant_id, src, rel, dst) WHERE valid_to IS NULL;
CREATE INDEX relation_dst_idx ON relation (tenant_id, dst) WHERE valid_to IS NULL;
CREATE INDEX relation_hist_idx ON relation (tenant_id, src, valid_from, valid_to);

-- Eventos (cambios de CI y relaciones, ejecuciones). Particionada por mes.
CREATE TABLE event (
    id        bigint GENERATED ALWAYS AS IDENTITY,
    tenant_id uuid        NOT NULL,
    ts        timestamptz NOT NULL DEFAULT now(),
    type      text        NOT NULL,
    ci_id     text,
    data      jsonb       NOT NULL DEFAULT '{}'::jsonb,
    PRIMARY KEY (id, ts)
) PARTITION BY RANGE (ts);
CREATE INDEX event_ci_idx   ON event (tenant_id, ci_id, ts DESC);
CREATE INDEX event_type_idx ON event (tenant_id, type, ts DESC);

-- Crea las particiones mensuales desde el mes actual hasta months_ahead meses después.
CREATE FUNCTION contexto_ensure_event_partitions(months_ahead int) RETURNS int
LANGUAGE plpgsql AS $$
DECLARE
    m date := date_trunc('month', now())::date;
    created int := 0;
    name text;
BEGIN
    FOR i IN 0..months_ahead LOOP
        name := format('event_y%sm%s', to_char(m, 'YYYY'), to_char(m, 'MM'));
        IF to_regclass(name) IS NULL THEN
            EXECUTE format('CREATE TABLE %I PARTITION OF event FOR VALUES FROM (%L) TO (%L)',
                           name, m, (m + interval '1 month')::date);
            created := created + 1;
        END IF;
        m := (m + interval '1 month')::date;
    END LOOP;
    RETURN created;
END $$;

-- Borra las particiones anteriores a keep_months meses (retención).
CREATE FUNCTION contexto_drop_event_partitions(keep_months int) RETURNS int
LANGUAGE plpgsql AS $$
DECLARE
    limit_date date := (date_trunc('month', now()) - make_interval(months => keep_months))::date;
    r record;
    dropped int := 0;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = 'event'::regclass AND c.relname ~ '^event_y\d{4}m\d{2}$'
          AND to_date(substr(c.relname, 8, 4) || substr(c.relname, 13, 2), 'YYYYMM') < limit_date
    LOOP
        EXECUTE format('DROP TABLE %I', r.relname);
        dropped := dropped + 1;
    END LOOP;
    RETURN dropped;
END $$;

SELECT contexto_ensure_event_partitions(3);

-- Ejecuciones del descubrimiento e informe de calidad de datos.
CREATE TABLE discovery_run (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   uuid        NOT NULL,
    started_at  timestamptz NOT NULL,
    finished_at timestamptz,
    status      text        NOT NULL CHECK (status IN ('running', 'ok', 'partial', 'failed')),
    instance    text        NOT NULL DEFAULT '',
    stats       jsonb       NOT NULL DEFAULT '{}'::jsonb,
    error       text
);
CREATE INDEX discovery_run_idx ON discovery_run (tenant_id, id DESC);

CREATE TABLE quality_issue (
    tenant_id uuid   NOT NULL,
    run_id    bigint NOT NULL REFERENCES discovery_run(id) ON DELETE CASCADE,
    kind      text   NOT NULL,
    severity  text   NOT NULL CHECK (severity IN ('info', 'warning', 'error')),
    subject   text   NOT NULL DEFAULT '',
    count     int    NOT NULL DEFAULT 1,
    detail    jsonb  NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX quality_issue_idx ON quality_issue (tenant_id, run_id);

-- Seguridad por filas: cada sesión fija app.tenant_id y solo ve sus filas.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['ci', 'ci_attr_history', 'relation', 'event', 'discovery_run', 'quality_issue'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format($p$CREATE POLICY tenant_isolation ON %I
                       USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
                       WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid)$p$, t);
    END LOOP;
END $$;
