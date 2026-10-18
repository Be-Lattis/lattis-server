-- Activité d'un classeur : dernier éditeur et dernier lecteur, avec la date.
--  * Dernière modification : ligne la plus récente de ses tableaux (index sur
--    lattis_xid) ou dernière suppression (pierres tombales) ; rien à écrire
--    de plus sur le chemin des écritures.
--  * Dernière lecture : l'application appelle lattis.record_open(workbook_id)
--    à l'ouverture d'un classeur ; une ligne par personne et par classeur.

CREATE TABLE lattis.workbook_reads (
    workbook_id uuid NOT NULL REFERENCES lattis.workbooks (id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    read_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (workbook_id, user_id)
);
CREATE INDEX workbook_reads_recent ON lattis.workbook_reads (workbook_id, read_at DESC);
REVOKE ALL ON lattis.workbook_reads FROM PUBLIC;
CREATE INDEX tombstones_recent ON lattis.tombstones (workbook_id, deleted_at DESC);

-- Note l'ouverture d'un classeur par session_user (accès vérifié).
CREATE FUNCTION lattis.record_open(p_workbook_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user uuid;
BEGIN
    PERFORM lattis.my_capabilities(p_workbook_id);
    SELECT id INTO v_user FROM lattis.users WHERE pg_role = session_user AND NOT disabled;
    IF v_user IS NULL THEN
        RETURN;
    END IF;
    INSERT INTO lattis.workbook_reads (workbook_id, user_id) VALUES (p_workbook_id, v_user)
    ON CONFLICT (workbook_id, user_id) DO UPDATE SET read_at = now();
END
$$;

-- Dernière modification d'un classeur : {"email", "at"} ou NULL.
CREATE FUNCTION lattis.workbook_last_edit(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks;
    t     record;
    v_by  text;
    v_at  timestamptz;
    b_by  text;
    b_at  timestamptz;
BEGIN
    SELECT * INTO v FROM lattis.workbooks WHERE id = p_workbook_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    FOR t IN SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
             WHERE n.nspname = v.schema_name AND c.relkind = 'r' AND c.relname NOT LIKE 'lattis\_meta\_%'
               AND EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = c.oid AND a.attname = 'lattis_xid' AND NOT a.attisdropped)
    LOOP
        EXECUTE format('SELECT lattis_updated_by, lattis_updated_at FROM %I.%I ORDER BY lattis_xid DESC LIMIT 1',
                       v.schema_name, t.relname) INTO v_by, v_at;
        IF v_at IS NOT NULL AND (b_at IS NULL OR v_at > b_at) THEN
            b_by := v_by; b_at := v_at;
        END IF;
    END LOOP;
    SELECT deleted_by, deleted_at INTO v_by, v_at FROM lattis.tombstones
     WHERE workbook_id = p_workbook_id ORDER BY deleted_at DESC LIMIT 1;
    IF v_at IS NOT NULL AND (b_at IS NULL OR v_at > b_at) THEN
        b_by := v_by; b_at := v_at;
    END IF;
    IF b_at IS NULL THEN
        RETURN NULL;
    END IF;
    RETURN jsonb_build_object('email', coalesce((SELECT email FROM lattis.users WHERE pg_role = b_by), b_by), 'at', b_at);
END
$$;
REVOKE EXECUTE ON FUNCTION lattis.workbook_last_edit(uuid) FROM PUBLIC;

CREATE OR REPLACE FUNCTION lattis.admin_workbooks() RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
BEGIN
    PERFORM lattis.require_server_owner();
    RETURN (SELECT coalesce(jsonb_agg(jsonb_build_object(
            'id', w.id, 'title', w.title, 'mode', w.mode, 'state', w.state, 'version', w.version,
            'schema', w.schema_name, 'created_by', w.created_by, 'created_at', w.created_at,
            'published_at', w.published_at,
            'last_edit', lattis.workbook_last_edit(w.id),
            'last_read', (SELECT jsonb_build_object('email', u.email, 'at', r.read_at)
                          FROM lattis.workbook_reads r JOIN lattis.users u ON u.id = r.user_id
                          WHERE r.workbook_id = w.id ORDER BY r.read_at DESC LIMIT 1),
            'owners', (SELECT coalesce(jsonb_agg(u.email ORDER BY u.email), '[]')
                       FROM lattis.roles r JOIN lattis.members m ON m.role_id = r.id
                       JOIN lattis.users u ON u.id = m.principal
                       WHERE r.workbook_id = w.id AND r.builtin = 'owner'),
            'members', (SELECT count(DISTINCT m.principal) FROM lattis.roles r
                        JOIN lattis.members m ON m.role_id = r.id WHERE r.workbook_id = w.id),
            'size', (SELECT coalesce(sum(pg_total_relation_size(c.oid)), 0) FROM pg_class c
                     JOIN pg_namespace n ON n.oid = c.relnamespace
                     WHERE n.nspname IN (w.schema_name, coalesce(w.staging_schema, '')) AND c.relkind IN ('r', 'p', 'm')))
        ORDER BY w.title), '[]') FROM lattis.workbooks w);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.record_open(uuid) TO PUBLIC;
