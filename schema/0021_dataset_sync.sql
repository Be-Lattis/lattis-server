-- Données d'entreprise : relevés d'un tableau lié.
--  * dataset_tombstones compare comme tombstones_since (xid >= since) : une
--    suppression de la transaction égale à sync_point n'est plus manquée.
--  * dataset_changed_ids : identifiants de toutes les lignes du tableau source
--    changées depuis since, visibles ou non. Une ligne changée qui n'est plus
--    rendue par la vue est sortie de l'audience (filtre devenu faux) : le
--    client la retire, sans tout recharger. Identifiants seulement, comme les
--    pierres tombales.

CREATE OR REPLACE FUNCTION lattis.dataset_tombstones(p_id uuid, p_since xid8) RETURNS TABLE (row_id uuid, xid xid8)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.dataset(p_id);
BEGIN
    IF NOT lattis.can_read_dataset(d) THEN
        RAISE EXCEPTION 'cette donnée ne vous est pas accessible' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN QUERY SELECT t.row_id, t.xid FROM lattis.tombstones t
                 WHERE t.table_id = d.table_id AND t.xid >= p_since ORDER BY t.xid;
END
$$;

CREATE FUNCTION lattis.dataset_changed_ids(p_id uuid, p_since xid8) RETURNS TABLE (row_id uuid, xid xid8)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    d lattis.datasets := lattis.dataset(p_id);
    v lattis.workbooks;
BEGIN
    IF NOT lattis.can_read_dataset(d) THEN
        RAISE EXCEPTION 'cette donnée ne vous est pas accessible' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT * INTO v FROM lattis.workbooks WHERE id = d.workbook_id;
    IF v.id IS NULL OR d.table_name IS NULL THEN
        RETURN;
    END IF;
    RETURN QUERY EXECUTE format('SELECT lattis_id, lattis_xid FROM %I.%I WHERE lattis_xid >= $1 ORDER BY lattis_xid',
                                v.schema_name, d.table_name) USING p_since;
END
$$;

GRANT EXECUTE ON FUNCTION lattis.dataset_changed_ids(uuid, xid8) TO PUBLIC;
