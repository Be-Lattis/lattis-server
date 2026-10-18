-- Audience d'une donnée par adresse : l'application ne connaît souvent que
-- l'adresse d'une personne. Adresse inconnue : P0002 (no_data_found), sans
-- créer de compte.
CREATE FUNCTION lattis.add_data_audience_by_email(p_dataset_id uuid, p_folder_id uuid, p_email text, p_predicate text) RETURNS uuid
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_user uuid;
BEGIN
    IF p_dataset_id IS NOT NULL THEN
        PERFORM lattis.require_manage_dataset(p_dataset_id);
    ELSE
        PERFORM lattis.require_data_publisher();
    END IF;
    SELECT id INTO v_user FROM lattis.users WHERE email = lower(trim(coalesce(p_email, ''))) AND NOT disabled
     ORDER BY last_login_at DESC NULLS LAST LIMIT 1;
    IF v_user IS NULL THEN
        RAISE EXCEPTION 'aucun compte pour %', lower(trim(coalesce(p_email, ''))) USING ERRCODE = 'no_data_found';
    END IF;
    RETURN lattis.add_data_audience(p_dataset_id, p_folder_id, v_user, p_predicate);
END
$$;

GRANT EXECUTE ON FUNCTION lattis.add_data_audience_by_email(uuid, uuid, text, text) TO PUBLIC;
