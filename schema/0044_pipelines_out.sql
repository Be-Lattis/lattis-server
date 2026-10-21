-- Pipelines OUT (export) : le tableau cible est lu par l'identité
-- technique (lectrice) et déversé vers l'extérieur avec les protocoles de
-- l'ingestion. source_kind désigne alors la destination : file (fichier
-- téléchargeable depuis l'administration), http, sftp ou sql ; format :
-- csv, json ou xlsx ; mode : replace ou append.

ALTER TABLE lattis.pipelines ADD COLUMN direction text NOT NULL DEFAULT 'in' CHECK (direction IN ('in', 'out'));

-- Sens d'un pipeline, posé juste après sa création.
CREATE FUNCTION lattis.set_pipeline_direction(p_id uuid, p_direction text) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    x lattis.pipelines;
BEGIN
    -- Propriétaires du serveur, ou gestionnaire d'équipe dans son périmètre.
    PERFORM lattis.require_pipeline_admin(p_id);
    SELECT * INTO x FROM lattis.pipelines WHERE id = p_id FOR UPDATE;
    IF p_direction NOT IN ('in', 'out') THEN
        RAISE EXCEPTION 'sens : in ou out' USING ERRCODE = 'invalid_parameter_value';
    END IF;
    IF p_direction = 'out' THEN
        IF x.source_kind NOT IN ('file', 'http', 'sftp', 'sql') THEN
            RAISE EXCEPTION 'export : destination fichier, http, sftp ou sql' USING ERRCODE = 'invalid_parameter_value';
        END IF;
        IF x.source_kind <> 'sql' AND x.format NOT IN ('csv', 'json', 'xlsx') THEN
            RAISE EXCEPTION 'export : format csv, json ou xlsx' USING ERRCODE = 'invalid_parameter_value';
        END IF;
        IF x.mode NOT IN ('replace', 'append') THEN
            RAISE EXCEPTION 'export : mode replace ou append' USING ERRCODE = 'invalid_parameter_value';
        END IF;
    END IF;
    UPDATE lattis.pipelines SET direction = p_direction, updated_at = now() WHERE id = p_id;
    PERFORM lattis.audit_log('set_pipeline_direction', p_id::text, NULL, jsonb_build_object('direction', p_direction));
END
$$;

GRANT EXECUTE ON FUNCTION lattis.set_pipeline_direction(uuid, text) TO PUBLIC;
