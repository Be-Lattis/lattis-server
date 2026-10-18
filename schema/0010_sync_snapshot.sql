-- Point de reprise après un rechargement complet (retour de l'application) :
-- sync_point() vaut la plus vieille transaction en cours ; une transaction
-- longue le maintient sous le plus grand xid purgé et since_covered(since)
-- resterait faux à chaque relevé. Le poste garde aussi l'instantané de son
-- dernier rechargement (pg_current_snapshot(), même transaction) : si la
-- dernière purge y est visible, toutes les suppressions purgées étaient déjà
-- prises en compte par ce rechargement.

ALTER TABLE lattis.sync_horizon ADD COLUMN purge_xid xid8 NOT NULL DEFAULT '0';

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
        UPDATE lattis.sync_horizon
           SET purged_xid = greatest(purged_xid, v_max), purge_xid = pg_current_xact_id();
    END IF;
    RETURN v_n;
END
$$;

-- p_reload : instantané du dernier rechargement complet du tableau (NULL :
-- aucun). Couvert si since dépasse le plus grand xid purgé, ou si la dernière
-- purge est antérieure au rechargement.
CREATE FUNCTION lattis.since_covered(p_since xid8, p_reload pg_snapshot) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT p_since = '0'::xid8 OR p_since > purged_xid
           OR (p_reload IS NOT NULL AND purge_xid <> '0'::xid8 AND pg_visible_in_snapshot(purge_xid, p_reload))
    FROM lattis.sync_horizon
$$;

GRANT EXECUTE ON FUNCTION lattis.since_covered(xid8, pg_snapshot) TO PUBLIC;
