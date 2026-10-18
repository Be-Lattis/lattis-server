-- Synchronisation (retours de l'application, docs/collaboration/synchronisation.md §4) :
--  * changes_since peut écarter les transactions du poste (ses échos) ;
--  * « *acl » sur le canal du classeur quand ses droits changent ;
--  * since_covered dit si les pierres tombales couvrent encore un point de reprise.

-- Deltas sans les transactions p_skip (identifiants lus par le poste avec
-- pg_current_xact_id() dans ses transactions d'écriture). Une ligne réécrite
-- ensuite par un autre revient, son lattis_xid ayant changé.
CREATE FUNCTION lattis.changes_since(p_workbook_id uuid, p_table_id uuid, p_since xid8, p_skip xid8[])
RETURNS TABLE (row_data jsonb, deleted boolean, xid xid8)
LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r record;
BEGIN
    r := lattis.table_ref(p_workbook_id, p_table_id);
    RETURN QUERY EXECUTE format(
        'SELECT to_jsonb(t), false, t.lattis_xid FROM %I.%I t WHERE t.lattis_xid >= $1 AND t.lattis_xid <> ALL ($2)',
        r.schema_name, r.table_name)
        USING p_since, coalesce(p_skip, '{}');
    RETURN QUERY SELECT jsonb_build_object('lattis_id', s.row_id), true, s.xid
                 FROM lattis.tombstones_since(p_workbook_id, p_table_id, p_since) s
                 WHERE s.xid <> ALL (coalesce(p_skip, '{}'));
END
$$;

GRANT EXECUTE ON FUNCTION lattis.changes_since(uuid, uuid, xid8, xid8[]) TO PUBLIC;

-- Changement de droits : rôles, niveaux, membres, restrictions. pg_notify
-- regroupe les charges identiques d'une même transaction : une seule
-- notification « *acl » par classeur et par transaction.
CREATE FUNCTION lattis.trg_acl_notify() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_row  jsonb := to_jsonb(CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END);
    v_wb   uuid := (v_row ->> 'workbook_id')::uuid;
    v_slug text;
BEGIN
    IF v_wb IS NULL THEN
        SELECT workbook_id INTO v_wb FROM lattis.roles WHERE id = (v_row ->> 'role_id')::uuid;
    END IF;
    SELECT slug INTO v_slug FROM lattis.workbooks WHERE id = v_wb;
    IF v_slug IS NOT NULL THEN
        PERFORM pg_notify('wb_' || v_slug, '*acl');
    END IF;
    RETURN NULL;
END
$$;

CREATE TRIGGER lattis_acl_notify AFTER INSERT OR UPDATE OR DELETE ON lattis.roles
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_acl_notify();
CREATE TRIGGER lattis_acl_notify AFTER INSERT OR UPDATE OR DELETE ON lattis.role_grants
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_acl_notify();
CREATE TRIGGER lattis_acl_notify AFTER INSERT OR UPDATE OR DELETE ON lattis.members
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_acl_notify();
CREATE TRIGGER lattis_acl_notify AFTER INSERT OR UPDATE OR DELETE ON lattis.restrictions
    FOR EACH ROW EXECUTE FUNCTION lattis.trg_acl_notify();

-- Pierres tombales : plus grand xid purgé. Un point de reprise est couvert
-- tant qu'il le dépasse (toute suppression d'xid ≥ since est encore là).
CREATE TABLE lattis.sync_horizon (
    singleton  boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    purged_xid xid8 NOT NULL DEFAULT '0'
);
INSERT INTO lattis.sync_horizon DEFAULT VALUES;

CREATE OR REPLACE FUNCTION lattis.purge_tombstones() RETURNS integer
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_n   integer;
    v_max xid8;
BEGIN
    PERFORM lattis.require_service();
    WITH gone AS (
        DELETE FROM lattis.tombstones
        WHERE deleted_at < now() - make_interval(secs => (lattis.config('tombstone_retention') #>> '{}')::double precision)
        RETURNING xid
    )
    SELECT count(*), max(xid) INTO v_n, v_max FROM gone;
    IF v_max IS NOT NULL THEN
        UPDATE lattis.sync_horizon SET purged_xid = greatest(purged_xid, v_max);
    END IF;
    RETURN v_n;
END
$$;

-- since couvert : les deltas depuis since sont complets. Sinon, recharger le
-- tableau en entier. 0 (premier chargement) est toujours couvert.
CREATE FUNCTION lattis.since_covered(p_since xid8) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT p_since = '0'::xid8 OR p_since > purged_xid FROM lattis.sync_horizon
$$;

GRANT EXECUTE ON FUNCTION lattis.since_covered(xid8) TO PUBLIC;
