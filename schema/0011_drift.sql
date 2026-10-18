-- Écarts (retour de l'application, docs/collaboration/droits.md §4) : une
-- règle ou un droit ajouté à la main entrait dans l'empreinte calculée par
-- compile_acl, et check_drift ne le signalait plus après une recompilation
-- ou un set_level, alors qu'il était toujours là. compile_acl ne retire pas
-- ce qui n'est pas à lui : l'empreinte ne porte plus que sur les règles
-- lattis_* et les droits des rôles du classeur, et check_drift signale
-- toujours les règles et droits étrangers.

-- Tableaux sans écart avec l'ancienne empreinte : ils restent sans écart.
CREATE TEMP TABLE drift_clean ON COMMIT DROP AS
SELECT f.workbook_id, f.table_id, w.schema_name, t.name
FROM lattis.acl_fingerprints f
JOIN lattis.workbooks w ON w.id = f.workbook_id
CROSS JOIN LATERAL lattis.data_tables(w.schema_name) t
WHERE t.table_id = f.table_id AND f.fingerprint = lattis.table_fingerprint(w.schema_name, t.name);

-- Droits d'un tableau donnés à d'autres que son propriétaire et les rôles de
-- classeur (PUBLIC compris).
CREATE FUNCTION lattis.foreign_grants(p_schema text, p_table text) RETURNS jsonb
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT coalesce(jsonb_agg(DISTINCT CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE a.grantee::regrole::text END), '[]')
    FROM pg_class c
    CROSS JOIN LATERAL (SELECT relacl AS acl UNION ALL
                        SELECT attacl FROM pg_attribute WHERE attrelid = c.oid AND attacl IS NOT NULL) l,
         aclexplode(l.acl) a
    WHERE c.oid = format('%I.%I', p_schema, p_table)::regclass
      AND a.grantee <> c.relowner
      AND (a.grantee = 0 OR NOT EXISTS (SELECT 1 FROM lattis.roles r WHERE r.pg_role = a.grantee::regrole::text))
$$;

CREATE OR REPLACE FUNCTION lattis.table_fingerprint(p_schema text, p_table text) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT md5(coalesce((
        SELECT string_agg(format('%s|%s|%s|%s|%s', policyname, permissive, cmd, roles::text,
                                 coalesce(qual, '') || '|' || coalesce(with_check, '')), E'\n' ORDER BY policyname)
        FROM pg_policies WHERE schemaname = p_schema AND tablename = p_table
          AND policyname LIKE 'lattis\_%'), '')
        || E'\n' || coalesce((
            SELECT string_agg(format('%s|%s|%s', a.grantee::regrole, a.privilege_type, a.is_grantable), E'\n'
                              ORDER BY a.grantee::regrole::text, a.privilege_type)
            FROM pg_class c, aclexplode(c.relacl) a
            WHERE c.oid = format('%I.%I', p_schema, p_table)::regclass
              AND a.grantee <> 0
              AND (a.grantee = c.relowner OR EXISTS (SELECT 1 FROM lattis.roles r WHERE r.pg_role = a.grantee::regrole::text))), '')
        || E'\n' || coalesce((
            SELECT string_agg(format('%s|%s|%s', t.attname, a.grantee::regrole, a.privilege_type), E'\n'
                              ORDER BY t.attname, a.grantee::regrole::text, a.privilege_type)
            FROM pg_attribute t, aclexplode(t.attacl) a
            WHERE t.attrelid = format('%I.%I', p_schema, p_table)::regclass AND t.attacl IS NOT NULL
              AND EXISTS (SELECT 1 FROM lattis.roles r WHERE r.pg_role = a.grantee::regrole::text)), '')
        || E'\n' || (SELECT relrowsecurity::text || relforcerowsecurity::text FROM pg_class
                     WHERE oid = format('%I.%I', p_schema, p_table)::regclass))
$$;

UPDATE lattis.acl_fingerprints f SET fingerprint = lattis.table_fingerprint(d.schema_name, d.name)
FROM drift_clean d WHERE d.workbook_id = f.workbook_id AND d.table_id = f.table_id;

CREATE OR REPLACE FUNCTION lattis.check_drift(p_workbook_id uuid) RETURNS jsonb
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', true);
    v_tab record;
    v_out jsonb := '[]';
    v_fp  text;
    v_pol jsonb;
    v_gr  jsonb;
BEGIN
    IF v.mode <> 'collaborative' OR v.state <> 'published' THEN
        RETURN v_out;
    END IF;
    FOR v_tab IN SELECT * FROM lattis.data_tables(v.schema_name) LOOP
        SELECT fingerprint INTO v_fp FROM lattis.acl_fingerprints WHERE workbook_id = v.id AND table_id = v_tab.table_id;
        SELECT coalesce(jsonb_agg(policyname ORDER BY policyname), '[]') INTO v_pol FROM pg_policies
        WHERE schemaname = v.schema_name AND tablename = v_tab.name AND policyname NOT LIKE 'lattis\_%';
        v_gr := lattis.foreign_grants(v.schema_name, v_tab.name);
        IF v_fp IS DISTINCT FROM lattis.table_fingerprint(v.schema_name, v_tab.name)
           OR v_pol <> '[]' OR v_gr <> '[]' THEN
            v_out := v_out || jsonb_build_object('table_id', v_tab.table_id, 'name', v_tab.name,
                'foreign_policies', v_pol, 'foreign_grants', v_gr);
        END IF;
    END LOOP;
    RETURN v_out;
END
$$;
