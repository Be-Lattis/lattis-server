-- Publication d'un gros tableau plus rapide : pas de déclencheurs par ligne
-- (horodatage, pierres tombales) tant que le classeur est en brouillon, ni
-- dans la version de travail d'une republication. Les valeurs par défaut des
-- colonnes système couvrent ce que posait le déclencheur à l'insertion
-- (auteur, position, transaction, date) ; finish_publish recompile les droits
-- et installe les déclencheurs sur la version publiée. Avant publication,
-- aucun poste ne synchronise le tableau ; une republication change de
-- version et les postes rechargent tout.
CREATE OR REPLACE FUNCTION lattis.install_triggers(v lattis.workbooks, p_schema text, p_table text, p_table_id uuid) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_chan text := 'wb_' || v.slug;
BEGIN
    IF p_table LIKE 'lattis\_meta\_%' THEN
        IF p_schema IS NOT DISTINCT FROM v.staging_schema THEN
            EXECUTE format('DROP TRIGGER IF EXISTS lattis_notify ON %I.%I', p_schema, p_table);
            RETURN;
        END IF;
        EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                        FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, '*meta');
        RETURN;
    END IF;
    IF v.state = 'draft' OR p_schema IS NOT DISTINCT FROM v.staging_schema THEN
        EXECUTE format('DROP TRIGGER IF EXISTS lattis_stamp ON %I.%I', p_schema, p_table);
        EXECUTE format('DROP TRIGGER IF EXISTS lattis_tombstone ON %I.%I', p_schema, p_table);
        EXECUTE format('DROP TRIGGER IF EXISTS lattis_notify ON %I.%I', p_schema, p_table);
        RETURN;
    END IF;
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_stamp BEFORE INSERT OR UPDATE ON %I.%I
                    FOR EACH ROW EXECUTE FUNCTION lattis.trg_stamp()', p_schema, p_table);
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_tombstone AFTER DELETE ON %I.%I
                    REFERENCING OLD TABLE AS old_rows FOR EACH STATEMENT
                    EXECUTE FUNCTION lattis.trg_tombstone(%L, %L)', p_schema, p_table, v.id, p_table_id);
    EXECUTE format('CREATE OR REPLACE TRIGGER lattis_notify AFTER INSERT OR UPDATE OR DELETE ON %I.%I
                    FOR EACH STATEMENT EXECUTE FUNCTION lattis.trg_notify(%L, %L)', p_schema, p_table, v_chan, p_table_id);
END
$$;
