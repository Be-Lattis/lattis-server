-- Synchronisation (audit du mode en ligne de l'application, 20/10/2026) :
--  * changes_since rend aussi, comme supprimées, les lignes modifiées depuis
--    le point de reprise que l'appelant ne voit plus (elles sortent de son
--    filtre de lecture ou d'une restriction) : elles quittaient sinon jamais
--    son poste, avec leur ancienne valeur ;
--  * une ligne supprimée ne peut pas être recréée sous le même identifiant
--    (renvoi d'un ajout dont la réponse s'était perdue, après sa suppression
--    par un collègue) : les postes divergeaient durablement ;
--  * save_meta avec la version attendue : deux concepteurs qui enregistrent
--    la mise en page au même moment ne s'écrasent plus (40001, le second
--    refait sa fusion).

-- Lignes supprimées : recherche par identifiant (déclencheur d'insertion).
CREATE INDEX tombstones_row ON lattis.tombstones (row_id);

-- Une ligne de cet identifiant a-t-elle été supprimée ? (SECURITY DEFINER :
-- les pierres tombales ne sont pas lisibles par les membres ; l'identifiant
-- est connu de l'appelant, qui l'insère.)
CREATE FUNCTION lattis.row_deleted(p_row_id uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM lattis.tombstones WHERE row_id = p_row_id)
$$;

GRANT EXECUTE ON FUNCTION lattis.row_deleted(uuid) TO PUBLIC;

CREATE OR REPLACE FUNCTION lattis.trg_stamp() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_device text := coalesce(current_setting('lattis.device', true), '');
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF lattis.row_deleted(NEW.lattis_id) THEN
            RAISE EXCEPTION 'ligne supprimée entre-temps par un autre membre : elle n''est pas recréée'
                USING ERRCODE = 'integrity_constraint_violation';
        END IF;
        NEW.lattis_author := current_user;
        NEW.lattis_pos := coalesce(NEW.lattis_pos, '');
        NEW.lattis_other_xid := NULL;
    ELSE
        IF NEW.lattis_id IS DISTINCT FROM OLD.lattis_id THEN
            RAISE EXCEPTION 'lattis_id ne se modifie pas' USING ERRCODE = 'check_violation';
        END IF;
        NEW.lattis_author := OLD.lattis_author;
        IF (OLD.lattis_updated_by, OLD.lattis_updated_device) IS DISTINCT FROM (current_user::text, v_device) THEN
            NEW.lattis_other_xid := OLD.lattis_xid;
        ELSE
            NEW.lattis_other_xid := OLD.lattis_other_xid;
        END IF;
    END IF;
    NEW.lattis_xid := pg_current_xact_id();
    NEW.lattis_updated_by := current_user;
    NEW.lattis_updated_device := v_device;
    NEW.lattis_updated_at := now();
    RETURN NEW;
END
$$;

-- Identifiants des lignes modifiées depuis p_since, sans les filtres de
-- l'appelant (SECURITY DEFINER) : seulement des identifiants, comme les
-- pierres tombales. changes_since en retire ceux que l'appelant voit.
CREATE FUNCTION lattis.changed_ids_since(p_workbook_id uuid, p_table_id uuid, p_since xid8)
RETURNS TABLE (row_id uuid, xid xid8)
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r record;
BEGIN
    r := lattis.table_ref(p_workbook_id, p_table_id);
    RETURN QUERY EXECUTE format('SELECT t.lattis_id, t.lattis_xid FROM %I.%I t WHERE t.lattis_xid >= $1', r.schema_name, r.table_name)
        USING p_since;
END
$$;

-- Deltas (0008) et lignes sorties de la vue de l'appelant : une ligne
-- modifiée depuis p_since qu'il ne voit pas lui est rendue supprimée (elle
-- a pu sortir de son filtre ; si elle n'y a jamais été, la supprimer en
-- local ne change rien). Les échos (p_skip) ne sont pas écartés ici : un
-- poste qui fait lui-même sortir une ligne de son filtre doit la retirer.
CREATE OR REPLACE FUNCTION lattis.changes_since(p_workbook_id uuid, p_table_id uuid, p_since xid8, p_skip xid8[])
RETURNS TABLE (row_data jsonb, deleted boolean, xid xid8)
LANGUAGE plpgsql STABLE SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    r record;
BEGIN
    r := lattis.table_ref(p_workbook_id, p_table_id);
    RETURN QUERY EXECUTE format(
        'SELECT to_jsonb(t), false, t.lattis_xid FROM %I.%I t WHERE t.lattis_xid >= $1
           AND (t.lattis_xid <> ALL ($2) OR t.lattis_other_xid >= $1)',
        r.schema_name, r.table_name)
        USING p_since, coalesce(p_skip, '{}');
    RETURN QUERY SELECT jsonb_build_object('lattis_id', s.row_id), true, s.xid
                 FROM lattis.tombstones_since(p_workbook_id, p_table_id, p_since) s
                 WHERE s.xid <> ALL (coalesce(p_skip, '{}'));
    RETURN QUERY EXECUTE format(
        'SELECT jsonb_build_object(''lattis_id'', h.row_id), true, h.xid
           FROM lattis.changed_ids_since($1, $2, $3) h
          WHERE NOT EXISTS (SELECT 1 FROM %I.%I t WHERE t.lattis_id = h.row_id)',
        r.schema_name, r.table_name)
        USING p_workbook_id, p_table_id, p_since;
END
$$;

-- Ancienne forme, sans p_skip (appelée par le client quand il n'a pas
-- d'échos à écarter) : même résultat.
CREATE OR REPLACE FUNCTION lattis.changes_since(p_workbook_id uuid, p_table_id uuid, p_since xid8)
RETURNS TABLE (row_data jsonb, deleted boolean, xid xid8)
LANGUAGE sql STABLE SET search_path = pg_catalog, pg_temp
AS $$
    SELECT * FROM lattis.changes_since(p_workbook_id, p_table_id, p_since, NULL::xid8[])
$$;

-- Mise en page avec la version attendue (lattis_xid lu par le client dans la
-- même transaction ; NULL : l'objet n'existait pas). Une autre écriture
-- validée entre-temps : 40001, le client relit et refait sa fusion.
CREATE FUNCTION lattis.save_meta(p_workbook_id uuid, p_kind text, p_payload jsonb, p_expected xid8) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v     lattis.workbooks := lattis.require_workbook_role(p_workbook_id, '{owner,designer}', false);
    v_sch text := lattis.publish_target(v);
    v_key text := coalesce(p_payload ->> 'key', '');
    v_cur xid8;
    v_n   integer;
BEGIN
    IF coalesce(p_kind, '') = '' OR p_payload IS NULL THEN
        RAISE EXCEPTION 'type et contenu requis' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    EXECUTE format('SELECT lattis_xid FROM %I.lattis_meta_objects WHERE kind = $1 AND key = $2 FOR UPDATE', v_sch)
        INTO v_cur USING p_kind, v_key;
    IF v_cur IS DISTINCT FROM p_expected THEN
        RAISE EXCEPTION 'mise en page modifiée entre-temps par un autre concepteur' USING ERRCODE = 'serialization_failure';
    END IF;
    IF v_cur IS NULL THEN
        EXECUTE format('INSERT INTO %I.lattis_meta_objects (kind, key, payload) VALUES ($1, $2, $3) ON CONFLICT (kind, key) DO NOTHING', v_sch)
            USING p_kind, v_key, p_payload;
        GET DIAGNOSTICS v_n = ROW_COUNT;
        IF v_n = 0 THEN
            RAISE EXCEPTION 'mise en page modifiée entre-temps par un autre concepteur' USING ERRCODE = 'serialization_failure';
        END IF;
        RETURN;
    END IF;
    EXECUTE format('UPDATE %I.lattis_meta_objects SET payload = $3, lattis_xid = pg_current_xact_id(),
                        lattis_updated_by = session_user, lattis_updated_at = now() WHERE kind = $1 AND key = $2', v_sch)
        USING p_kind, v_key, p_payload;
END
$$;

GRANT EXECUTE ON FUNCTION lattis.changed_ids_since(uuid, uuid, xid8), lattis.save_meta(uuid, text, jsonb, xid8) TO PUBLIC;
