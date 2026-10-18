# Contrat entre l'application Lattis et lattisd

Ce que l'application de bureau (`lattis-app`) doit savoir pour se connecter à un Serveur Lattis. Complète les §7 et §11 de la [spécification](specification.md).

## 1. Découverte

`GET <url>/.well-known/lattis.json` (public, mis en cache 60 s) :

```json
{
  "name": "Serveur Acme",
  "logo": "https://…/logo.svg",
  "version": "0.1.0",
  "min_client_version": "0.0.0",
  "edition": "admin",
  "access_mode": "invitation",
  "issuer": "https://lattis.acme.fr",
  "client_id": "lattis-desktop",
  "scope": "openid lattis",
  "authorization_endpoint": "https://lattis.acme.fr/oauth/authorize",
  "token_endpoint": "https://lattis.acme.fr/oauth/token",
  "revocation_endpoint": "https://lattis.acme.fr/oauth/revoke",
  "providers": [{ "id": "…uuid…", "label": "Compte Lattis", "kind": "password", "icon": "password" }],
  "postgres": { "database": "lattis", "sslmode": "verify-full",
                "rw": { "host": "pg.acme.fr", "port": 5432 }, "ro": [] }
}
```

`kind` ∈ `password`, `google`, `google_workspace`, `microsoft`, `keycloak`, `oidc` ; `icon` reprend `kind` (icônes côté client).

## 2. Connexion (RFC 8252)

1. Ouvrir dans le navigateur du système `authorization_endpoint` avec `response_type=code`, `client_id`, `redirect_uri=http://127.0.0.1:<port>/callback` (port quelconque ; `[::1]` et `localhost` acceptés), `scope=openid lattis`, `code_challenge` (S256, obligatoire), `state`, `nonce` facultatif, et `provider=<id ou kind>` pour sauter la page de choix.
2. Retour sur la redirection avec `code`, `state` et `iss` (RFC 9207, à comparer à `issuer`), ou `error`.
3. `POST token_endpoint` (formulaire) : `grant_type=authorization_code`, `client_id`, `code`, `redirect_uri`, `code_verifier`. Réponse :

```json
{ "access_token": "…", "token_type": "Bearer", "expires_in": 900,
  "refresh_token": "…", "id_token": "…", "scope": "openid lattis",
  "pg_role": "louis@acme.fr" }
```

4. Rafraîchir avant expiration : `grant_type=refresh_token`, `client_id`, `refresh_token`. Le jeton de rafraîchissement **change à chaque usage** : enregistrer le nouveau dans le trousseau. Présenter un ancien jeton révoque toute la session (détection de réutilisation).
5. Déconnexion : `POST revocation_endpoint` avec `token=<refresh_token>`.

## 3. Connexion PostgreSQL

- pgx v5.11 gère `OAUTHBEARER` : `cfg.OAuthTokenProvider = func(ctx) (string, error) { return accessToken, nil }` (vérifié contre PostgreSQL 18.6, test `internal/e2e`).
- Utilisateur PostgreSQL = `pg_role` de la réponse. Il contient souvent `@` : le poser dans `cfg.User`, **pas dans une URL**.
- `sslmode` et points `rw`/`ro` : ceux de la découverte. TLS obligatoire.
- `application_name` = `Lattis/<version>/<usage>` (§13).
- Un jeton refusé (expiré, autre rôle, compte désactivé) donne une erreur d'authentification : rafraîchir puis réessayer une fois.

### 3.1 Réplicas (`ro`)

- Même jeton, même `pg_role`, même RLS que sur `rw` ; toute écriture y est refusée (`25006`, transaction en lecture seule).
- La liste `ro` de la découverte ne contient que les réplicas qui suivent le parent (retard sous `LATTIS_REPLICA_MAX_LAG`, 10 s par défaut). Elle est mise en cache 10 s au plus : la relire à chaque (re)connexion. Liste vide : tout lire sur `rw`.
- Lecture de ses propres écritures : après une écriture, lire sur `rw`, ou attendre que `pg_last_wal_replay_lsn()` du réplica atteigne `pg_current_wal_lsn()` lu sur `rw` après la validation.
- Deltas sur un réplica : `sync_point()` et `changes_since` y fonctionnent (vérifié contre PostgreSQL 18, test `internal/e2e`). Un `since` lu sur un serveur plus avancé reste valable sur un réplica en retard : les lignes manquantes reviennent au relevé suivant.
- **Refus inattendu sur un réplica** : une session ouverte pendant que le réplica rejoue la création ou la modification d'un classeur peut garder une vue périmée des rôles de l'utilisateur (cache de PostgreSQL, observé sur PostgreSQL 18) et refuser l'accès (`42501`, « aucun accès à ce classeur ») alors que `rw` l'accorde. Règle : sur un `42501` venu d'un réplica pour un classeur que `rw` donne accessible, fermer cette connexion et en ouvrir une nouvelle (ou lire sur `rw`) ; ne pas réessayer sur la même session.

## 4. Après connexion

- `SELECT lattis.login_summary()` : utilisateur, propriétaire du serveur ou non, classeurs (vide tant que les classeurs ne sont pas livrés).
- Un propriétaire du serveur administre avec les fonctions `lattis.*` sous sa propre identité (journalisées dans `lattis.audit`).

## 5. Classeurs

Toutes les fonctions sont dans le schéma `lattis`, appelées sous l'identité de l'utilisateur.

| Besoin | Appel |
|---|---|
| Liste « Ouvrir depuis un serveur » | `my_workbooks() → jsonb` : `id`, `title`, `mode`, `state`, `version`, `schema`, `roles` |
| Droits sur un classeur | `my_capabilities(workbook_id) → jsonb` : `title`, `mode`, `version`, `schema`, `channel` (canal `LISTEN`), `roles`, `can_design`, `can_manage`, `custom_roles`, `tables` (`table_id` → `name`, `sheet_id`, `level`, `filtered`, `read_filtered`, `write_filtered`), `restrictions` (`table_id`, `command`, `label`) |
| Publier | `create_workbook(title, 'frozen'\|'collaborative')`, puis `create_table(workbook_id, name, columns)` par tableau (`columns` : `[{"name", "type", "local_id", "table_local_id"}]` ; `table_local_id`, identifiant local du tableau, est lu dans le premier élément, types `text`, `integer`, `decimal`, `date`, `datetime`, `boolean`), copie des lignes par `COPY` dans `"<schema>"."<tableau>"` (`lattis_id` UUIDv7 et `lattis_pos` fournis par le client), `save_meta(workbook_id, kind, payload)`, `publish_script(workbook_id, name, source)`, puis `finish_publish(workbook_id)`. `abort_publish` abandonne. |
| Republier (figé) | `republish(workbook_id) → schéma de travail`, mêmes appels que la publication (ils visent la version de travail), puis `finish_publish` : la nouvelle version remplace l'ancienne d'un coup, notification `*version:<n>` sur le canal. La version de travail ne notifie rien : les lecteurs ne reçoivent que `*version:<n>`. |
| Inviter | `invite(email, workbook_id, '{reader}')` (propriétaire du classeur) |
| Supprimer | `drop_workbook(workbook_id)` |

Colonnes système de chaque tableau : `lattis_id`, `lattis_pos`, `lattis_xid`, `lattis_author`, `lattis_updated_by`, `lattis_updated_at`, `lattis_updated_device`, `lattis_other_xid`. Toutes sauf les deux premières sont posées par le serveur ; `lattis_id` et `lattis_author` ne se modifient pas.

## 6. Synchronisation (classeur collaboratif)

- `LISTEN "<channel>"` sur `rw`. Charge : identifiant du tableau modifié, `*meta` (mise en page ou structure), `*version:<n>` (republication), `*acl` (rôles, niveaux, membres ou restrictions du classeur changés : relire `my_capabilities`). Jamais de données.
- Lecture des deltas, dans une transaction `REPEATABLE READ` : `SELECT lattis.sync_point()` (à mémoriser comme prochain `since`), puis `SELECT row_data, deleted, xid FROM lattis.changes_since(workbook_id, table_id, since)`. `row_data` est la ligne en JSON (colonnes par leur nom) ou `{"lattis_id": …}` quand `deleted`. Une même ligne peut revenir deux fois : appliquer de façon idempotente.
- Échos : `changes_since(workbook_id, table_id, since, skip xid8[])` écarte les lignes et suppressions dont la dernière écriture vient d'une des transactions `skip`. Le poste lit `pg_current_xact_id()` dans chacune de ses transactions d'écriture et passe ceux qui sont postérieurs à `since`. Une ligne réécrite ensuite par un autre revient (son `lattis_xid` a changé). Une ligne dont le poste a recouvert l'écriture d'un autre faite depuis `since` (pas encore reçue) revient aussi : le serveur garde la dernière écriture d'un autre auteur (`lattis_other_xid`). Auteur = rôle de l'utilisateur et poste : chaque poste pose `SET lattis.device = '<identifiant du poste>'` sur ses connexions d'écriture, sinon deux postes du même utilisateur passent pour un seul.
- Colonnes système ajoutées : `lattis_updated_device` (poste de la dernière écriture, vide s'il n'est pas posé) et `lattis_other_xid` (voir ci-dessus), posées par le serveur.
- Point de reprise trop ancien : `lattis.since_covered(since) → boolean`, dans la même transaction que les deltas. Faux : des pierres tombales ont été purgées depuis `since`, recharger le tableau en entier.
- Écriture : une transaction par étape d'annulation, `UPDATE … WHERE lattis_id = $1 AND "<col>" IS NOT DISTINCT FROM $vu`. Zéro ligne touchée → `lattis.why_not(workbook_id, table_id, row_id) → jsonb` : `{"reason": "modified", "row", "updated_by", "updated_at"}`, `{"reason": "deleted", "deleted_by", "deleted_at"}`, `{"reason": "forbidden"}` ou `{"reason": "missing"}`.
- Un `INSERT`/`UPDATE`/`DELETE` refusé pour tout le tableau (lecteur) lève `insufficient_privilege` (42501).
- Un `INSERT` ou un `UPDATE` refusé par un filtre d'écriture (`WITH CHECK`) lève `42501` et défait la transaction. Pour ne refuser que l'opération fautive, l'encadrer d'un point de sauvegarde (`SAVEPOINT op` … `RELEASE` / `ROLLBACK TO op`) : avec pgx, les trois ordres partent dans le même lot (`pgx.Batch`), sans aller-retour de plus.
- Ordre : deux insertions au même endroit peuvent porter la même `lattis_pos` ; départager par `lattis_id`, comme le serveur (`ORDER BY lattis_pos, lattis_id`). `why_not` ne distingue pas un déplacement refusé : « modified » rend la ligne actuelle, `lattis_pos` compris.
- Pierres tombales gardées 30 jours (`tombstone_retention`) : au-delà, recharger le tableau en entier.

## 7. Droits avancés (classeur collaboratif, propriétaire du classeur)

| Besoin | Appel |
|---|---|
| Écran « Droits d'un classeur » | `workbook_acl(workbook_id) → jsonb` : `roles` (`id`, `name`, `builtin`, `grants`, `members`), `restrictions`, `invitations` en attente |
| Rôle personnalisé | `create_role(workbook_id, name) → uuid` (aucun droit tant qu'aucun niveau n'est posé), `drop_role(role_id)`, `grant_role(role_id, user_id)`, `revoke_role(role_id, user_id)` |
| Niveau d'un rôle | `set_level(role_id, 'workbook'\|'sheet'\|'table', node_id, 'none'\|'reader'\|'participant'\|'editor', read_filter, write_filter)`. Le niveau d'un nœud s'hérite vers le bas ; `NULL` comme niveau retire celui du nœud. Filtres : expressions SQL booléennes sur les colonnes du tableau (`"Région" = 'Nord'`), tableaux seulement. |
| Restriction | `add_restriction(workbook_id, table_id, 'select'\|'insert'\|'update'\|'delete'\|'all', role_ids, predicate, label) → uuid` (`role_ids` vide = tous les rôles du classeur sauf propriétaires et concepteurs), `drop_restriction(id)`. Combinées en ET avec les niveaux. |
| Écarts | `check_drift(workbook_id) → jsonb` : tableaux dont les règles ou droits ont changé hors de Lattis, avec `foreign_policies`. `compile_acl(workbook_id)` (propriétaire ou concepteur) recompile et les remet d'aplomb, sans republier. |
| Inviter à un rôle personnalisé | `invite(email, workbook_id, roles)` : `roles` mêle rôles fournis (`reader`…) et noms de rôles personnalisés (sans casse). Une adresse inconnue reçoit un compte en attente : `grant_role` vaut ensuite pour elle aussi. |
| Membres (propriétaire ou concepteur) | `workbook_members(workbook_id) → jsonb` : `[{"user_id", "email", "display_name", "pg_role", "roles"}]`, pour choisir qui voir dans « Voir en tant que… ». |
| Tableau rattaché à une feuille | `create_table(workbook_id, name, columns, sheet_id)` : `set_level(…, 'sheet', sheet_id, …)` s'applique à ses tableaux. Sans `sheet_id`, le tableau n'hérite que du classeur. |

`my_capabilities` : `read_filtered` vaut vrai quand un filtre de lecture ou une restriction `select`/`all` me vise (je ne vois peut-être pas toutes les lignes), `write_filtered` quand un filtre d'écriture ou une restriction `insert`/`update`/`delete`/`all` me vise. `filtered` (l'un ou l'autre filtre de niveau) reste pour compatibilité.

Pas encore : groupes d'annuaire et équipes comme membres (`members.principal` ne désigne que des utilisateurs).

Un filtre refusé (point-virgule, `$`, commentaire, parenthèses déséquilibrées, colonne inconnue, expression non booléenne) lève `invalid_parameter_value` (22023) avec la raison.

**« Voir en tant que… »** : `POST <preview_endpoint>` (découverte), en-tête `Authorization: Bearer <jeton d'accès>` (le même que pour PostgreSQL), corps `{"workbook_id", "user_id", "table_id"}`. Réponse `{"rows": [...], "truncated": bool}` : au plus 1 000 lignes, celles que verrait l'utilisateur, lues en lecture seule sous son rôle. Réservé aux propriétaires et concepteurs du classeur (403 sinon), journalisé. 401 : jeton refusé, 404 : classeur, tableau ou utilisateur inconnu.
