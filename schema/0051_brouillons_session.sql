-- Brouillons de session (décidé par Louis le 02/11/2026) : la session de
-- l'application web s'enregistre en continu sur le serveur, sans volume par
-- utilisateur. Le moteur appelle ces fonctions sous le rôle de la personne,
-- sur sa connexion au parent : chacun ne voit que ses brouillons. La démo
-- publique n'a pas de connexion à PostgreSQL, donc pas de brouillon.
--
-- data : archive .lattis, déjà compressée (stockage EXTERNAL, sans seconde
-- compression). Chaque enregistrement réécrit l'archive entière, dans le WAL
-- et donc vers les réplicas : le moteur attend la fin d'un enregistrement
-- avant le suivant, et en saute s'il prend du retard. Table journalisée : un
-- brouillon survit à un arrêt brutal de PostgreSQL et à une bascule.

CREATE TABLE lattis.session_drafts (
    user_id    uuid NOT NULL REFERENCES lattis.users (id) ON DELETE CASCADE,
    id         uuid NOT NULL,
    title      text NOT NULL DEFAULT '',
    data       bytea NOT NULL,
    size       bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, id)
) WITH (autovacuum_vacuum_scale_factor = 0.02, autovacuum_vacuum_threshold = 50,
        toast.autovacuum_vacuum_scale_factor = 0.02, toast.autovacuum_vacuum_threshold = 50);
ALTER TABLE lattis.session_drafts ALTER COLUMN data SET STORAGE EXTERNAL;

-- Limite par personne, tous brouillons confondus (0 : sans limite).
INSERT INTO lattis.server_config (key, value) VALUES ('draft_quota_mb', '256');

-- La personne connectée (compte actif).
CREATE FUNCTION lattis.draft_owner() RETURNS uuid
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_me uuid := (SELECT id FROM lattis.users WHERE pg_role = session_user AND NOT disabled);
BEGIN
    IF v_me IS NULL THEN
        RAISE EXCEPTION 'réservé aux utilisateurs Lattis' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN v_me;
END
$$;

-- Crée ou remplace un brouillon ; LT001 au-delà de la limite de la personne
-- (le brouillon précédent reste alors intact).
CREATE FUNCTION lattis.draft_save(p_id uuid, p_title text, p_data bytea) RETURNS timestamptz
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v_me    uuid := lattis.draft_owner();
    v_quota bigint := coalesce((lattis.config('draft_quota_mb') #>> '{}')::bigint, 0) * 1048576;
    v_size  bigint := octet_length(p_data);
    v_used  bigint;
    v_at    timestamptz := clock_timestamp();
BEGIN
    IF p_id IS NULL OR p_data IS NULL THEN
        RAISE EXCEPTION 'brouillon : identifiant et contenu obligatoires' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Enregistrements simultanés d'une même personne : limite comptée juste.
    PERFORM pg_advisory_xact_lock(hashtextextended('lattis.session_drafts:' || v_me::text, 0));
    IF v_quota > 0 THEN
        SELECT coalesce(sum(size), 0) INTO v_used FROM lattis.session_drafts WHERE user_id = v_me AND id <> p_id;
        IF v_used + v_size > v_quota THEN
            RAISE EXCEPTION 'brouillons au maximum : % Mo utilisés avec celui-ci, limite de % Mo',
                ceil((v_used + v_size) / 1048576.0)::bigint, v_quota / 1048576
                USING ERRCODE = 'LT001';
        END IF;
    END IF;
    INSERT INTO lattis.session_drafts (user_id, id, title, data, size, updated_at)
    VALUES (v_me, p_id, left(coalesce(p_title, ''), 500), p_data, v_size, v_at)
    ON CONFLICT (user_id, id) DO UPDATE
        SET title = EXCLUDED.title, data = EXCLUDED.data, size = EXCLUDED.size, updated_at = EXCLUDED.updated_at;
    RETURN v_at;
END
$$;

-- Brouillons de la personne, du plus récent au plus ancien, sans le contenu.
CREATE FUNCTION lattis.draft_list() RETURNS TABLE (id uuid, title text, size bigint, updated_at timestamptz)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT d.id, d.title, d.size, d.updated_at FROM lattis.session_drafts d
    WHERE d.user_id = lattis.draft_owner() ORDER BY d.updated_at DESC
$$;

-- Contenu d'un brouillon de la personne ; no_data_found s'il n'existe pas.
CREATE FUNCTION lattis.draft_read(p_id uuid) RETURNS bytea
LANGUAGE plpgsql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    v bytea;
BEGIN
    SELECT data INTO v FROM lattis.session_drafts WHERE user_id = lattis.draft_owner() AND id = p_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'brouillon inconnu' USING ERRCODE = 'no_data_found';
    END IF;
    RETURN v;
END
$$;

-- Retire un brouillon de la personne (sans erreur s'il n'existe plus).
CREATE FUNCTION lattis.draft_delete(p_id uuid) RETURNS void
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    DELETE FROM lattis.session_drafts WHERE user_id = lattis.draft_owner() AND id = p_id
$$;

REVOKE EXECUTE ON FUNCTION lattis.draft_owner() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION lattis.draft_save(uuid, text, bytea), lattis.draft_list(), lattis.draft_read(uuid),
    lattis.draft_delete(uuid) TO PUBLIC;
