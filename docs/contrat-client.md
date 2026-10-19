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

`source_url` : code source du serveur (AGPL-3.0, article 13), à proposer dans « À propos » (`LATTIS_SOURCE_URL`).

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

- `SELECT lattis.login_summary()` : utilisateur, propriétaire du serveur ou non (`server_owner`), rôle « créateur de classeurs » (`workbook_creator`), droit de créer un classeur (`can_create_workbooks`), classeurs (vide tant que les classeurs ne sont pas livrés).
- `can_create_workbooks` décide si l'application propose « Publier sur le serveur » : vrai si le serveur laisse tout le monde publier (`workbook_creators = everyone`), ou si la personne est propriétaire du serveur ou créateur de classeurs. Sinon `create_workbook` répond `42501`.
- Un propriétaire du serveur administre avec les fonctions `lattis.*` sous sa propre identité (journalisées dans `lattis.audit`).

## 5. Classeurs

Toutes les fonctions sont dans le schéma `lattis`, appelées sous l'identité de l'utilisateur.

| Besoin | Appel |
|---|---|
| Liste « Ouvrir depuis un serveur » | `my_workbooks() → jsonb` : `id`, `title`, `mode`, `state`, `version`, `schema`, `roles` |
| Ouverture d'un classeur | `record_open(workbook_id)` à chaque ouverture (et au plus une fois par heure tant qu'il reste ouvert) : alimente « Dernière lecture » de l'administration. Sans accès : `42501`. |
| Droits sur un classeur | `my_capabilities(workbook_id) → jsonb` : `title`, `mode`, `version`, `schema`, `channel` (canal `LISTEN`), `roles`, `can_design`, `can_manage`, `custom_roles`, `tables` (`table_id` → `name`, `sheet_id`, `level`, `filtered`, `read_filtered`, `write_filtered`), `restrictions` (`table_id`, `command`, `label`) |
| Publier | `create_workbook(title, 'frozen'\|'collaborative')`, puis `create_table(workbook_id, name, columns)` par tableau (`columns` : `[{"name", "type", "local_id", "table_local_id"}]` ; `table_local_id`, identifiant local du tableau, est lu dans le premier élément, types `text`, `integer`, `decimal`, `date`, `datetime`, `boolean`), copie des lignes par `COPY` dans `"<schema>"."<tableau>"` (`lattis_id` UUIDv7 et `lattis_pos` fournis par le client), `save_meta(workbook_id, kind, payload)`, `publish_script(workbook_id, name, source)`, puis `finish_publish(workbook_id)`. `abort_publish` abandonne. |
| Republier (figé) | `republish(workbook_id) → schéma de travail`, mêmes appels que la publication (ils visent la version de travail), puis `finish_publish` : la nouvelle version remplace l'ancienne d'un coup, notification `*version:<n>` sur le canal. La version de travail ne notifie rien : les lecteurs ne reçoivent que `*version:<n>`. |
| Inviter | `invite(email, workbook_id, '{reader}')` (propriétaire du classeur) |
| Supprimer | `drop_workbook(workbook_id)` |

Colonnes système de chaque tableau : `lattis_id`, `lattis_pos`, `lattis_xid`, `lattis_author`, `lattis_updated_by`, `lattis_updated_at`, `lattis_updated_device`, `lattis_other_xid`. Toutes sauf les deux premières sont posées par le serveur ; `lattis_id` et `lattis_author` ne se modifient pas.

## 6. Synchronisation (classeur collaboratif)

- `LISTEN "<channel>"` sur `rw`. Charge : identifiant du tableau modifié, `*meta` (mise en page ou structure), `*structure:<table_id>` (colonnes ajoutées, renommées, supprimées ou converties, tableau renommé ou supprimé : recharger ce tableau en entier), `*version:<n>` (republication), `*acl` (rôles, niveaux, membres ou restrictions du classeur changés : relire `my_capabilities`). Jamais de données.
- Lecture des deltas, dans une transaction `REPEATABLE READ` : `SELECT lattis.sync_point()` (à mémoriser comme prochain `since`), puis `SELECT row_data, deleted, xid FROM lattis.changes_since(workbook_id, table_id, since)`. `row_data` est la ligne en JSON (colonnes par leur nom) ou `{"lattis_id": …}` quand `deleted`. Une même ligne peut revenir deux fois : appliquer de façon idempotente.
- Échos : `changes_since(workbook_id, table_id, since, skip xid8[])` écarte les lignes et suppressions dont la dernière écriture vient d'une des transactions `skip`. Le poste lit `pg_current_xact_id()` dans chacune de ses transactions d'écriture et passe ceux qui sont postérieurs à `since`. Une ligne réécrite ensuite par un autre revient (son `lattis_xid` a changé). Une ligne dont le poste a recouvert l'écriture d'un autre faite depuis `since` (pas encore reçue) revient aussi : le serveur garde la dernière écriture d'un autre auteur (`lattis_other_xid`). Auteur = rôle de l'utilisateur et poste : chaque poste pose `SET lattis.device = '<identifiant du poste>'` sur ses connexions d'écriture, sinon deux postes du même utilisateur passent pour un seul.
- Colonnes système ajoutées : `lattis_updated_device` (poste de la dernière écriture, vide s'il n'est pas posé) et `lattis_other_xid` (voir ci-dessus), posées par le serveur.
- Point de reprise trop ancien : `lattis.since_covered(since) → boolean`, dans la même transaction que les deltas. Faux : des pierres tombales ont été purgées depuis `since`, recharger le tableau en entier.
- Après un rechargement complet : garder `pg_current_snapshot()` lu dans la transaction du rechargement (avec `sync_point()`), et tester ensuite `lattis.since_covered(since, snapshot) → boolean`. Une transaction longue retient `sync_point()` sous l'horizon de purge ; le serveur tient alors `since` pour couvert si la dernière purge est visible dans cet instantané (le rechargement a déjà vu ces suppressions). Remplace tout point de contrôle côté poste ; l'instantané est à oublier dès que `since_covered(since)` seul redevient vrai.
- Écriture : une transaction par étape d'annulation, `UPDATE … WHERE lattis_id = $1 AND "<col>" IS NOT DISTINCT FROM $vu`. Zéro ligne touchée → `lattis.why_not(workbook_id, table_id, row_id) → jsonb` : `{"reason": "modified", "row", "updated_by", "updated_at"}`, `{"reason": "deleted", "deleted_by", "deleted_at"}`, `{"reason": "forbidden"}` ou `{"reason": "missing"}`.
- Un `INSERT`/`UPDATE`/`DELETE` refusé pour tout le tableau (lecteur) lève `insufficient_privilege` (42501).
- Un `INSERT` ou un `UPDATE` refusé par un filtre d'écriture (`WITH CHECK`) lève `42501` et défait la transaction. Pour ne refuser que l'opération fautive, l'encadrer d'un point de sauvegarde (`SAVEPOINT op` … `RELEASE` / `ROLLBACK TO op`) : avec pgx, les trois ordres partent dans le même lot (`pgx.Batch`), sans aller-retour de plus.
- Ordre : deux insertions au même endroit peuvent porter la même `lattis_pos` ; départager par `lattis_id`, comme le serveur (`ORDER BY lattis_pos, lattis_id`). `why_not` ne distingue pas un déplacement refusé : « modified » rend la ligne actuelle, `lattis_pos` compris.
- Pierres tombales gardées 30 jours (`tombstone_retention`) : au-delà, recharger le tableau en entier.

## 7. Droits avancés (classeur collaboratif, propriétaire du classeur)

| Besoin | Appel |
|---|---|
| Écran « Droits d'un classeur » | `workbook_acl(workbook_id) → jsonb` : `roles` (`id`, `name`, `builtin`, `grants`, `members`, `teams` : `[{"team_id", "name"}]`), `restrictions`, `invitations` en attente |
| Rôle personnalisé | `create_role(workbook_id, name) → uuid` (aucun droit tant qu'aucun niveau n'est posé), `drop_role(role_id)`, `grant_role(role_id, user_id ou team_id)`, `revoke_role(role_id, user_id ou team_id)` |
| Niveau d'un rôle | `set_level(role_id, 'workbook'\|'sheet'\|'table', node_id, 'none'\|'reader'\|'participant'\|'editor', read_filter, write_filter)`. Le niveau d'un nœud s'hérite vers le bas ; `NULL` comme niveau retire celui du nœud. Filtres : expressions SQL booléennes sur les colonnes du tableau (`"Région" = 'Nord'`), tableaux seulement. |
| Restriction | `add_restriction(workbook_id, table_id, 'select'\|'insert'\|'update'\|'delete'\|'all', role_ids, predicate, label) → uuid` (`role_ids` vide = tous les rôles du classeur sauf propriétaires et concepteurs), `drop_restriction(id)`. Combinées en ET avec les niveaux. |
| Écarts | `check_drift(workbook_id) → jsonb` : tableaux dont les règles ou droits ont changé hors de Lattis, avec `foreign_policies` (règles hors `lattis_*`) et `foreign_grants` (droits donnés à d'autres que les rôles du classeur, `PUBLIC` compris). `compile_acl(workbook_id)` (propriétaire ou concepteur) recompile et remet d'aplomb les règles et droits générés, sans republier ; il ne retire pas les règles et droits étrangers, qui restent signalés tant qu'ils sont là. |
| Inviter à un rôle personnalisé | `invite(email, workbook_id, roles)` : `roles` mêle rôles fournis (`reader`…) et noms de rôles personnalisés (sans casse). Une adresse inconnue reçoit un compte en attente : `grant_role` vaut ensuite pour elle aussi. |
| Membres (propriétaire ou concepteur) | `workbook_members(workbook_id) → jsonb` : `[{"user_id", "email", "display_name", "pg_role", "roles", "teams"}]`, pour choisir qui voir dans « Voir en tant que… ». Les membres par une équipe y sont aussi : `roles` réunit leurs rôles en personne et par leurs équipes, `teams` nomme les équipes par lesquelles ils sont membres. |
| Tableau rattaché à une feuille | `create_table(workbook_id, name, columns, sheet_id)` : `set_level(…, 'sheet', sheet_id, …)` s'applique à ses tableaux. Sans `sheet_id`, le tableau n'hérite que du classeur. |

`my_capabilities` : `read_filtered` vaut vrai quand un filtre de lecture ou une restriction `select`/`all` me vise (je ne vois peut-être pas toutes les lignes), `write_filtered` quand un filtre d'écriture ou une restriction `insert`/`update`/`delete`/`all` me vise. `filtered` (l'un ou l'autre filtre de niveau) reste pour compatibilité.

**Équipes.** Créées par les propriétaires du serveur (administration web, écran « Équipes », ou `create_team(name) → uuid`, `rename_team(id, name)`, `drop_team(id)`, `add_team_member(team_id, user_id)`, `remove_team_member(team_id, user_id)`). Pour tous :
- `list_teams() → jsonb` : `[{"id", "name", "parent_id", "depth", "path", "member_count", "subteam_count", "members"}]`, dans l'ordre de l'arbre (`path` : « Ventes › Ventes Nord »), où `members` (membres directs) est `null` sauf pour un propriétaire du serveur. C'est la liste où choisir une équipe dans « Partager ».
- Sous-équipes, sur autant de niveaux qu'on veut (`set_team_parent(team_id, parent_id)`, `create_team(name, parent_id)`, propriétaires du serveur ; cycles refusés en `22023`) : un rôle donné à une équipe vaut pour toutes ses sous-équipes, `my_teams()` rend aussi les équipes parentes, et `workbook_members` nomme l'équipe qui porte le rôle.
- Une équipe est membre d'un rôle comme une personne : `grant_role(role_id, team_id)`. Ses membres reçoivent le rôle tant qu'ils sont dans l'équipe. `my_capabilities`, les règles et « Voir en tant que… » en tiennent compte sans rien de plus.
- Un membre ajouté ou retiré d'une équipe envoie `*acl` sur le canal de chaque classeur où l'équipe a un rôle.
- « Mon équipe » dans un filtre : `"Équipe" IN (SELECT lattis.my_teams())`. `my_teams()` rend les noms des équipes de la personne qui lit, et il est permis dans les filtres et les restrictions.
- Le nom d'une équipe est unique, sans casse. Le renommer change ce que rend `my_teams()`, donc les filtres qui comparent une colonne à ce nom.
- Les groupes d'annuaire ne sont pas encore traités. Ils pourront alimenter ces équipes plus tard.

Un filtre refusé (point-virgule, `$`, commentaire, parenthèses déséquilibrées, colonne inconnue, expression non booléenne) lève `invalid_parameter_value` (22023) avec la raison.

**« Voir en tant que… »** : `POST <preview_endpoint>` (découverte), en-tête `Authorization: Bearer <jeton d'accès>` (le même que pour PostgreSQL), corps `{"workbook_id", "user_id", "table_id"}`. Réponse `{"rows": [...], "truncated": bool}` : au plus 1 000 lignes, celles que verrait l'utilisateur, lues en lecture seule sous son rôle. Réservé aux propriétaires et concepteurs du classeur (403 sinon), journalisé. 401 : jeton refusé, 404 : classeur, tableau ou utilisateur inconnu.

## 8. Lien vers un classeur

- Adresse à partager : `<issuer>/w/<workbook_id>`. L'écran Partager l'affiche, prête à copier.
- Dans le navigateur, `lattisd` connecte la personne si besoin, vérifie son accès sous sa propre identité (`my_capabilities`), puis propose d'ouvrir le classeur dans l'application de bureau ou, en édition `web`, dans le navigateur. Un classeur inexistant et un classeur interdit donnent la même page (403), sans titre. Le choix est retenu par le navigateur (cookie `lattis_open`, « Changer » pour revenir au choix).
- « Ouvrir dans Lattis » ouvre `lattis://open?server=<issuer encodé>&workbook=<workbook_id>`. L'application déclare le schéma `lattis` à l'installation (Windows : registre `HKCU\Software\Classes\lattis` ; Linux : fichier `.desktop` avec `MimeType=x-scheme-handler/lattis` ; macOS : `CFBundleURLTypes`). À l'appel : si l'instance tourne déjà, lui transmettre l'adresse ; vérifier que `server` est un serveur Lattis (découverte §1) et demander confirmation avant de se connecter à un serveur jamais utilisé ; se connecter si besoin (§2), puis ouvrir le classeur comme « Ouvrir depuis un serveur » (§5). Aucun autre paramètre n'est lu.

## 9. Data Lake Lattis

Un tableau d'un classeur publié se partage dans un catalogue rangé en dossiers. Il est lisible depuis n'importe quel classeur par son audience (personnes, équipes avec leurs sous-équipes, filtre facultatif par entrée).

**Partager** (publieur de données ou propriétaire du serveur, et propriétaire ou concepteur du classeur source) :

| Besoin | Appel |
|---|---|
| Droit de partager | `login_summary()` : `can_publish_data` (et `data_publisher`) |
| Dossiers | `create_data_folder(name, parent_id) → id`, `rename_data_folder(id, name)`, `move_data_folder(id, parent_id)` (cycles refusés, `22023`), `drop_data_folder(id)` (vide seulement, `55000`) |
| Partager un tableau | `create_dataset(workbook_id, table_id, folder_id, name, description, sql_name, hidden_columns text[]) → id`. `sql_name` vide : proposé à partir du nom (`clients`, `clients_2`…), sinon `^[a-z][a-z0-9_]{0,62}$` et libre. Il ne change plus ensuite. |
| Modifier | `update_dataset(id, {"name", "description", "folder_id", "hidden_columns", "inherit_audience"})` (champs facultatifs) |
| Audience | `add_data_audience(dataset_id, folder_id, principal, predicate) → id` : l'un des deux identifiants, `principal` = utilisateur ou équipe, `predicate` = filtre comme ceux des rôles (`"Région" = 'Nord'`, `"Équipe" IN (SELECT lattis.my_teams())`), vide = toutes les lignes. L'audience d'un dossier vaut pour les données qu'il contient, sous-dossiers compris, si `inherit_audience` (vrai par défaut). `add_data_audience_by_email(dataset_id, folder_id, email, predicate) → id` : même chose avec l'adresse d'une personne (adresse sans compte : erreur `P0002`). `remove_data_audience(id)`. |
| Retirer | `drop_dataset(id)` : la vue disparaît, `NOTIFY lattis_data '*withdrawn:<id>'`. |

**Lire** (toute personne de l'audience) :

- `data_catalog() → jsonb` : `{"can_publish", "folders": [{"id", "parent_id", "name", "path", "depth", "audience"}], "datasets": [{"id", "folder_id", "name", "description", "sql_name", "readable", "manageable", "available", "table_id", "channel", "columns": [{"name", "type"}], "updated_at", "source", "audience", "used_by"}]}`. Une personne hors publieurs ne voit que les données qu'elle peut lire et les dossiers qui y mènent. `source`, `audience` et `used_by` (classeurs qui l'utilisent, avec leur dernière modification et leur dernière lecture) ne sont remplis que pour qui gère la donnée. `available` faux : source supprimée ou tableau introuvable (la donnée reste au catalogue).
- Lignes : `SELECT … FROM donnees."<sql_name>"`, sur `rw` ou `ro`. Colonnes : `lattis_id`, `lattis_pos`, `lattis_xid`, `lattis_updated_at`, puis les colonnes exposées. Sans droit : `42501`.
- **Tableau lié** (lecture seule, à jour en direct) : chargement complet, puis comme un tableau collaboratif. `LISTEN "<channel>"` et relevé à chaque notification dont la charge vaut `table_id` (ou `*version:<n>` : rechargement complet, le classeur source a été republié et `table_id` a changé, relire `data_catalog`) ; lignes changées `SELECT … FROM donnees."<sql_name>" WHERE lattis_xid > $since`, supprimées `dataset_tombstones(id, since) → (row_id, xid)` (`xid >= since`), sorties de l'audience : identifiants de `dataset_changed_ids(id, since) → (row_id, xid)` (toutes les lignes changées de la source, visibles ou non) absents des lignes changées lues dans la vue, à retirer localement ; prochain `since` = `sync_point()` pris avant la lecture. `LISTEN lattis_data` : `<dataset_id>` (vue refaite : colonnes ou audience ont pu changer, relire le catalogue), `*catalog` (dossiers ou données ajoutés, déplacés, renommés), `*withdrawn:<id>` (partage retiré : garder la dernière copie en lecture seule, marquée « partage retiré »).
- Un classeur du serveur qui insère un tableau lié l'annonce : `link_dataset(dataset_id, workbook_id)` (propriétaire ou concepteur du classeur), `unlink_dataset` quand le tableau lié est supprimé. C'est ce qui remplit « Utilisée par ».

### 9.1 Publier un tableau seul (source poussée)

Depuis un classeur **local**, sans publier le classeur : « Partager au Data Lake Lattis… » propose soit de publier tout le classeur (§5, gestion des droits), soit de publier un seul tableau comme source du Data Lake. Dans ce second cas, le serveur crée une source poussée : un classeur technique (`kind = 'source'`, absent de `my_workbooks` et des listes de classeurs) qui ne contient que ce tableau, un pipeline de type `push` qui l'alimente, et la donnée du catalogue. Seul le contenu du tableau part sur le serveur ; l'audience se règle ensuite comme pour toute donnée (`add_data_audience…`).

Publieur de données ou propriétaire du serveur (`login_summary().can_publish_data`) ; le droit de créer des classeurs n'est pas demandé.

| Besoin | Appel |
|---|---|
| Créer | `create_pushed_source(p jsonb) → {"pipeline_id", "dataset_id", "workbook_id", "table_id", "sql_name"}`. `p` : `name` (nom de la donnée), `description`, `folder_id`, `sql_name` (facultatif, règles de `create_dataset`), `table_name` (facultatif, `name` sinon), `columns` (`[{"name", "type"}]`, types de `create_table`), `mode` (`replace` par défaut : le tableau du serveur devient celui envoyé ; `append` : ajout à la suite ; `upsert` : mise à jour par clé, avec `key_columns`), `key_columns`, `origin` (libre, gardé pour l'affichage : `{"workbook": "Ventes.lattis", "workbook_local_id", "table": "Clients", "table_local_id"}`). L'application garde `pipeline_id` dans les métadonnées du tableau local. |
| Envoyer le contenu | `POST <émetteur>/api/push/<pipeline_id>?wait=60`, `Authorization: Bearer <jeton d'accès>` (le même que pour PostgreSQL), `Content-Type: text/csv`, corps : CSV en UTF-8, première ligne = noms des colonnes du tableau (séparateur `;` ou `,`, décimales `,` ou `.`, dates `AAAA-MM-JJ` ou `JJ/MM/AAAA`, booléens `true`/`false`), 100 Mo au plus. Réponse `200` : bilan de l'exécution `{"status": "ok"\|"warning"\|"error", "rows_read", "inserted", "updated", "deleted", "rejected", "rejects": [{"row", "column", "value", "error"}], "error", "trigger": "push", "started_at", "finished_at", "id"}` (`id` : identifiant de l'exécution) ; `202 {"status": "running", "run_id"}` si l'écriture dure plus que `wait` secondes (0 à 300) ; `401` jeton refusé ; `403` pas propriétaire de la source ; `404` source inconnue ; `409` un envoi précédent n'est pas fini ou source suspendue ; `413` trop gros. |
| Suivre un envoi | `GET <émetteur>/api/push/<pipeline_id>?run=<run_id>` (même jeton) ou `pushed_run(pipeline_id, run_id)` : le bilan, `{"id", "status": "pending"}` tant qu'elle n'a pas commencé, `404` inconnue. |
| Modifier | `update_pushed_source(pipeline_id, {"mode", "key_columns", "enabled"})` (champs facultatifs ; créateur ou propriétaire du serveur ; `upsert` demande une clé). |
| État | `GET <émetteur>/api/push/<pipeline_id>` (même jeton) ou `my_pushed_sources() → [{"pipeline_id", "name", "mode", "key_columns", "origin", "workbook_id", "table_id", "enabled", "pending", "datasets": [{"id", "name", "sql_name"}], "last_run"}]` |
| Colonnes changées dans le tableau local | Fonctions de structure (§9.3 de la spécification) sur `workbook_id`/`table_id` de la source : `add_column`, `insert_column`, `rename_column`, `drop_column`, `alter_column_type` (l'auteur est propriétaire du classeur technique). La donnée du catalogue suit. |
| Retirer | `drop_pushed_source(pipeline_id)` : donnée retirée (`*withdrawn:<id>`), pipeline et classeur technique supprimés. |

Au catalogue, la donnée d'une source poussée porte, pour qui la gère, `source.kind = "source"`, `source.pipeline_id` et `source.origin` (classeur et tableau d'origine) ; `source.title` reste le nom du classeur technique (« Source : <nom> »).

Quand envoyer : à la demande (« Mettre à jour la source »), et au choix de l'application à l'enregistrement du classeur local. Les écritures passent sous l'identité technique du pipeline (« Pipeline : <nom> », visible dans `lattis_author`) ; l'administration les montre dans Pipelines, avec la source d'origine.

## 10. Formulaires

Un tableau d'un classeur collaboratif publié se partage comme une page `<émetteur>/f/<code>` servie par lattisd (conception côté application : `docs/conception/partager-comme-formulaire.md`). Chaque réponse devient une ligne du tableau, écrite sous l'identité technique du formulaire (« Formulaire : <titre> », `lattis_author`), qui ne peut qu'ajouter des lignes et relire ou modifier celles qu'elle a écrites. Ce rôle n'apparaît ni parmi les rôles ni parmi les membres ; une restriction « tous les rôles » le vise aussi. Les colonnes sont citées par leur `column_id` (`lattis_meta_columns`).

| Besoin | Appel |
|---|---|
| Disponibilité | `forms_settings() → {"forms": true, "forms_public"}` (fonction absente : `42883`, serveur sans formulaires) |
| Créer | `create_form(workbook_id, table_id, p) → {"id", "code", "path", "url"}` (propriétaire ou concepteur ; brouillon). `p` : `definition`, `access` (`members` par défaut, `audience`, `server_accounts`, `public`), `audience` (identifiants de personnes ou d'équipes), `opens_at`, `closes_at`, `max_responses`, `max_per_respondent` (`null` : sans limite), `edit` (`none`, `until_close`, `7 days`), `daily_limit` (1 000). Erreurs : `55000` classeur figé ou non publié, `P0002` tableau inconnu, `22023` définition invalide (`DETAIL` = `{"fields": {"<chemin ou column_id>": "raison"}}`), `42501` lien public non permis. |
| Modifier | `update_form(id, p, version) → {"version"}` (champs facultatifs de `p`) ; `40001` : modifié entre-temps. |
| État | `set_form_state(id, 'open'\|'paused'\|'closed')` ; `open` revérifie la définition sur la structure actuelle (`22023`). |
| Supprimer | `drop_form(id)` : identité désactivée, rôle interne supprimé, lignes gardées. |
| Lister | `list_forms(workbook_id)` : `[{"id", "code", "path", "url", "table_id", "title", "state", "pause_reason", "access", "manageable", "version", "responses", "last_response_at", "definition", "max_per_respondent", "audience": [{"principal", "kind", "name"}], "options"}]` ; chiffres, définition, version et audience pour les propriétaires et concepteurs seulement ; les autres membres ne voient que les formulaires ouverts auxquels ils peuvent répondre. |
| Statistiques | `form_stats(id, days) → {"total", "last_at", "throttled", "by_day": [{"day", "n"}]}` |
| Aperçu | `form_preview_url(id) → "/f/<code>?p=<jeton>"`, valable 10 minutes, ouvert même en brouillon ; les envois ne sont pas enregistrés. |
| Audience | `form_audience_add(id, principal)`, `form_audience_add_by_email(id, email) → user_id` (`P0002` : pas de compte), `form_audience_remove(id, principal)` |
| Images | `POST <émetteur>/api/forms/<id>/images?kind=logo\|banner`, jeton d'accès en `Bearer`, corps PNG ou JPEG (5 Mo au plus) → `{"id", "content_type", "bytes"}` ; l'image est réduite (logo 512 px, bannière 1600 × 600) et réencodée (métadonnées retirées). `401`, `403`, `404`, `413`, `415`. La définition cite `appearance.logo` et `appearance.banner` par cet identifiant. |

**Définition** (`format` 1) : `lang` (`fr`, `en`), `appearance` (`title` obligatoire, `intro`, `logo`, `banner`, `accent` : `green`, `teal`, `blue`, `indigo`, `purple`, `red`, `orange`, `slate`, `submit_label`, `thanks`, `another`, `closed_message`, `privacy`), `fields` et `server_values`.

- Champ : `{"column", "label", "help", "placeholder", "required", "widget", "default", "min", "max", "step", "max_length", "format", "unit", "list", "param", "locked", "hidden"}`. `widget` : vide ou `list` (selon le type et la liste : boutons jusqu'à 7 valeurs, liste au-delà), `text`, `textarea`, `number`, `date`, `datetime`, `checkbox`, `yesno`. `format` : `free`, `email`, `phone`, `url`. `list` : `{"values": […]}` (avec `copied_at`, `source` libres) ou `{"table_id", "column"}` (colonne d'un tableau du classeur, relue à chaque affichage, 1 000 valeurs au plus). `default` : `{"token": "today"\|"now"\|"year"\|"respondent"}` ou `{"fixed": …}`. `param` : nom court (`^[a-z][a-z0-9_]{0,39}$`) pour pré-remplir par l'adresse (`/f/<code>?site=Nord`) ; `locked` : valeur du lien imposée, affichée en lecture seule et reprise de l'adresse à l'envoi. `hidden` : valeur par défaut, sans champ.
- Intertitre : `{"kind": "section", "title", "description"}`.
- Position : `{"kind": "position", "latitude", "longitude"}` (deux colonnes décimales) ou `{"kind": "position", "column"}` (texte « lat, lon ») ; rempli par le bouton « Ma position » du navigateur, jamais automatiquement.
- `server_values` : `{"column", "value": {"token": "received_at"\|"respondent"\|"form_title"\|"next_number"}}` ou `{"column", "value": {"fixed": …}}`. `next_number` : maximum + 1 sous verrou du tableau.
- `intro`, `privacy` et les descriptions d'intertitres sont en Markdown réduit (gras, italique, liens `https`, listes) ; `thanks` : gras et italique. Le reste est du texte.

**Page** (`/f/<code>`) : HTML d'abord pour téléphone, envoi sans JavaScript ; le script ajoute la position, le brouillon gardé sur l'appareil et « Réessayer » sans réseau. Modes membres, audience et tout compte : connexion par la session du site (`/admin/login?next=/f/<code>`). Contrôles à l'envoi : jeton signé (anti-CSRF, délai minimal de 3 s, 24 h au plus), pot de miel, 64 Kio, 10 envois par minute et par adresse, limite quotidienne par formulaire, plafond, réponses par personne, types, listes, bornes, champs obligatoires et clé (`23505` : « Une valeur saisie existe déjà »). Réponses : `303` vers `/f/<code>/merci` ; `400` page avec les erreurs sous chaque champ ; `404` inconnu ou interdit (même page) ; `409` déjà répondu ou clé prise ; `410` fermé, pas encore ouvert ou complet ; `429`.

**Notifications** : `*form:<id>` sur le canal du classeur à la création, à la modification, au changement d'état (pause automatique comprise : `pause_reason` `spike`, `column_dropped`, `type_changed`, `manual`) et à la suppression. Les réponses ne notifient que l'identifiant du tableau, comme toute écriture.

**Structure** : renommer une colonne ne change rien ; supprimer une colonne retire ses champs et met le formulaire en pause si le champ était obligatoire ou posé par le serveur ; convertir une colonne revérifie la définition (pause si elle n'est plus valide) ; supprimer le tableau supprime ses formulaires.

**Administration** (propriétaires du serveur) : page Formulaires (tous les formulaires, accès, état, débit, pause et reprise) et réglage du lien public (`forms_public`, coupé par défaut).

## 11. Changements relayés par lattisd (jalon 8)

Au lieu d'un `LISTEN` par poste sur le parent, le poste suit ses classeurs par un flux d'événements servi par lattisd, qui écoute PostgreSQL une seule fois pour tous les postes (spécification §21, jalon 8). Le poste n'a alors plus de connexion permanente au parent : il lit sur un réplica et n'ouvre sur `rw` que des connexions courtes pour écrire (à fermer après quelques secondes sans écriture). Le `LISTEN` direct (§6) reste accepté pendant la transition.

- **Flux** : `GET <émetteur>/api/events?workbooks=<id>,<id>&data=1`, `Authorization: Bearer <jeton d'accès>` (le même que pour PostgreSQL), réponse `text/event-stream`. 1 à 200 classeurs, `data=1` pour les notifications du Data Lake (`lattis_data`). `401` jeton refusé, `403` un classeur inconnu ou interdit (accès vérifié comme `my_capabilities`), `400` paramètres. Pour changer la liste des classeurs suivis : rouvrir le flux. Un commentaire `: ping` part toutes les 25 s.
- **Événements** (`event:` puis `data:` en JSON) :
  - `ready` `{"workbooks", "data"}` : les canaux sont écoutés ; tout changement validé après ce point arrivera. Le poste lit alors ce qui a changé depuis sa dernière synchronisation (§6), puis suit les événements.
  - `change` `{"workbook", "payload", "lsn"}` : `payload` est la charge du `NOTIFY` du classeur (identifiant de tableau, `*meta`, `*structure:<table_id>`, `*version:<n>`, `*acl`, `*form:<id>`), `lsn` la position WAL du parent juste après le changement.
  - `data` `{"payload", "lsn"}` : charge de `lattis_data` (`<dataset_id>`, `*catalog`, `*withdrawn:<id>`).
  - `reset` `{}` : des événements ont pu se perdre (lattisd a dû rouvrir son écoute, ou le poste lisait trop lentement) : resynchroniser tous les classeurs suivis.
  - `revoked` `{"workbook"}` : le poste a perdu l'accès au classeur (après `*acl`) ; il n'en reçoit plus les changements.
- **Lire sur un réplica** : avant de lire les deltas sur `ro`, attendre que le réplica ait rejoué la position reçue : `SELECT pg_last_wal_replay_lsn() >= '<lsn>'::pg_lsn` (quelques dizaines de millisecondes d'ordinaire ; au-delà de quelques secondes, lire sur `rw`).
- **Reconnexion** : à la coupure du flux, rouvrir (avec un délai croissant), puis resynchroniser depuis le dernier `since` comme après `reset`.
- **Plusieurs lattisd** : chacun relaie tous les changements, le flux peut donc s'ouvrir sur n'importe lequel ; le répartiteur de charge doit seulement garder la connexion ouverte (pas de délai d'inactivité inférieur à 30 s, pas de mise en tampon des réponses).
