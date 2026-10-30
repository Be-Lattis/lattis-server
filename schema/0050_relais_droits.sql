-- Relais des changements (/api/events) : l'accès d'une personne à un
-- classeur est vérifié par la connexion de service de lattisd, plus sous
-- l'identité de la personne. Avant, chaque abonnement et chaque
-- revérification après « *acl » ouvraient une connexion PostgreSQL par
-- utilisateur (audit collaboration du 30/10/2026, S5).
--
-- Même règle que lattis.my_capabilities : membre (directement ou par une
-- équipe) d'un rôle du classeur, compte actif. Rend le canal du classeur,
-- NULL sans accès.
CREATE FUNCTION lattis.can_follow(p_user_id uuid, p_workbook_id uuid) RETURNS text
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
    SELECT 'wb_' || w.slug
    FROM lattis.workbooks w, lattis.users u
    WHERE w.id = p_workbook_id AND u.id = p_user_id AND NOT u.disabled
      AND EXISTS (SELECT 1 FROM lattis.roles r
                  WHERE r.workbook_id = w.id AND pg_has_role(u.pg_role, r.pg_role, 'MEMBER'))
$$;

REVOKE EXECUTE ON FUNCTION lattis.can_follow(uuid, uuid) FROM PUBLIC;
