# Serveur Lattis : spécification

*Version 0.1 du 18/10/2026. Issue de la note de conception `conception/collaboration-postgresql.md` du projet Lattis et des décisions prises par Louis le 18/10/2026. Les points encore non vérifiés sont marqués **[à vérifier]** et repris au §20.*

## 1. Objet

Le Serveur Lattis rend Lattis collaboratif. C'est un serveur **préconfiguré** qui réunit :
- **PostgreSQL 18**, source de vérité des classeurs partagés, dont les comptes, les droits et la sécurité au niveau des lignes (RLS) portent toute l'autorisation ;
- **`lattisd`**, un service Go qui fait l'identification (connexion multi-fournisseurs), l'inscription des utilisateurs, l'administration, les mesures, et en option l'application web.

Les postes de bureau Lattis travaillent sur une **copie locale SQLite** (cache) et parlent **directement à PostgreSQL** sous l'identité de l'utilisateur. `lattisd` ne s'intercale pas dans le flux de données des postes de bureau.

### 1.1 Hors périmètre

- Le moteur de calcul (formules, TCD) : il tourne sur le poste ou, en version web, dans `lattisd`, jamais en SQL côté serveur (publication de colonnes calculées côté serveur : évolution ultérieure).
- Les offres PostgreSQL gérées (Azure, AWS, Google Cloud) : elles n'acceptent pas de module d'authentification tiers. Le Serveur Lattis est auto-hébergé (chez le client ou par Be Lattis).
- L'édition hors ligne (prévue plus tard ; la consultation hors ligne est gérée par le client).

## 2. Vocabulaire

| Terme | Sens |
|---|---|
| Classeur partagé | Classeur dont les données, la structure et la mise en page vivent sur le serveur. Deux modes : **figé** et **collaboratif** (§6). |
| Tableau | Une table PostgreSQL du classeur. |
| Utilisateur | Une personne identifiée ; un rôle PostgreSQL `LOGIN` par utilisateur. |
| Fournisseur | Moyen de connexion : mot de passe Lattis, Google, Google Workspace, Microsoft, Keycloak, OIDC générique. |
| Tenant | Sous-ensemble autorisé d'un fournisseur : domaine Workspace, tenant Microsoft (`tid`), realm Keycloak, liste d'adresses Google. |
| Rôle Lattis | Définition nommée de droits sur un classeur (§8), traduite en rôle de groupe PostgreSQL `NOLOGIN`. |
| Restriction | Règle qui retire des droits par-dessus les rôles (RLS restrictive). |
| Parent / réplica | Serveur PostgreSQL principal (écritures) / serveurs en lecture (réplication en flux). |

## 3. Architecture

```
                       ┌──────────────────────────── Serveur Lattis ─────────────────────────────┐
 Poste de bureau       │                                                                          │
 ┌──────────────┐ HTTPS│  ┌──────────────────────────────┐        ┌───────────────────────────┐  │
 │ Lattis       │──────┼─▶│ lattisd                       │  SQL   │ PostgreSQL 18 (parent)    │  │
 │ (SQLite      │      │  │ - émetteur OIDC (§7)          │───────▶│ - schéma lattis           │  │
 │  local)      │      │  │ - inscription, administration │        │ - schémas wb_* (classeurs)│  │
 │              │ PG/TLS  │ - /metrics, santé             │        │ - module lattis_oauth     │  │
 │              │──────┼──┼──────────────────────────────▶│        └─────────────┬─────────────┘  │
 └──────────────┘      │  │ - (option) application web    │                      │ réplication    │
 Navigateur            │  └──────────────────────────────┘        ┌─────────────▼─────────────┐  │
 ┌──────────────┐ HTTPS│                                          │ Réplicas en lecture (0..n)│  │
 │ Lattis web   │──────┼─▶ lattisd (moteur Lattis par session) ──▶│                           │  │
 └──────────────┘      │                                          └───────────────────────────┘  │
                       └──────────────────────────────────────────────────────────────────────────┘
```

Composants livrés :

| Composant | Langage | Rôle |
|---|---|---|
| `lattisd` | Go | Service HTTP(S) : identification, inscription, administration, mesures, application web (option). |
| `lattis_oauth` | C (ou Rust/pgrx) | Module de validation OAuth de PostgreSQL 18 (`oauth_validator_libraries`). Ne vérifie que les jetons émis par `lattisd` (§7.3). |
| `schema/` | SQL | Migrations du schéma `lattis` (catalogue, fonctions d'administration, déclencheurs). |
| Image `be-lattis/server` | Docker | PostgreSQL 18 + `lattis_oauth` + migrations ; `lattisd` dans une image séparée `be-lattis/lattisd`. |
| `deploy/` | YAML | Docker Compose (parent + réplicas + lattisd) et chart Helm autonome pour Kubernetes. |

## 4. Deux versions

Le même binaire `lattisd` se déploie dans l'une de deux versions, choisies par configuration :

1. **Gestion des rôles** (`LATTIS_EDITION=admin`) : `lattisd` sert l'identification, l'inscription, les mesures et **une interface web d'administration** (utilisateurs, invitations, fournisseurs, tenants, rôles, restrictions, classeurs). Les utilisateurs travaillent avec l'application de bureau.
2. **Application web** (`LATTIS_EDITION=web`) : tout ce qui précède, plus **l'application Lattis complète dans le navigateur**, avec un comportement identique à la version de bureau (§14). Le scripting y est désactivé par défaut et s'active explicitement.

L'interface d'administration et l'application appellent les **fonctions SQL `lattis.*`** (§9), utilisables aussi directement par un DBA. Aucune règle d'autorisation n'existe ailleurs qu'en base.

## 5. Modèle de données

### 5.1 Schéma `lattis` (catalogue commun)

| Table | Contenu |
|---|---|
| `server_config` | Clé/valeur : nom affiché de la source (« Serveur Acme »), logo, mode d'accès (`invitation` ou `open`), rôle par défaut des arrivants, scripting web autorisé, durées de jetons. |
| `identity_providers` | `id`, `kind` (`password`, `google`, `google_workspace`, `microsoft`, `keycloak`, `oidc`), `label`, `issuer`, `client_id`, `client_secret_ref` (référence vers un secret, jamais la valeur), `scopes`, `enabled`, `position`. |
| `allowed_tenants` | `provider_id`, `kind` (`domain`, `tid`, `realm`, `email`, `any`), `value`, `default_role` (surcharge facultative). |
| `users` | `id` (uuid), `pg_role` (nom du rôle PostgreSQL), `email`, `display_name`, `provider_id`, `subject` (`sub` OIDC), `created_at`, `last_login_at`, `disabled`. Unicité (`provider_id`, `subject`). |
| `local_credentials` | Comptes à mot de passe gérés par `lattisd` : `user_id`, hachage Argon2id, date de changement, verrouillage. |
| `invitations` | `email`, `workbook_id` (facultatif), `roles[]`, `invited_by`, `expires_at`, `accepted_at`. |
| `workbooks` | `id`, `schema_name` (`wb_<slug>`), `title`, `mode` (`frozen`, `collaborative`), `version` (classeur figé), `created_by`, `created_at`. |
| `roles` | `id`, `workbook_id`, `name`, `builtin` (`owner`, `designer`, `editor`, `participant`, `reader` ou NULL), `pg_role`. |
| `role_grants` | `role_id`, `node_kind` (`workbook`, `sheet`, `table`), `node_id`, `level` (`none`, `reader`, `participant`, `editor`), `read_filter` (SQL, facultatif), `write_filter` (SQL, facultatif). |
| `members` | `role_id`, `principal` (`user_id`), `granted_by`, `granted_at`. |
| `teams`, `team_members`, `team_roles` | Équipes Lattis (`id`, `name`, rôle PostgreSQL `NOLOGIN` `lattis_team_<id>`), leurs membres, et les rôles de classeur donnés à une équipe. Les groupes d'annuaire pourront les alimenter. |
| `restrictions` | `id`, `table_id`, `command` (`select`, `insert`, `update`, `delete`, `all`), `applies_to` (rôles, vide = tous), `predicate` (SQL), `label`. |
| `tombstones` | `workbook_id`, `table_id`, `row_id` (uuid), `xid` (`xid8`), `deleted_by`, `deleted_at`. |
| `audit` | Journal des actions d'administration (qui, quoi, quand, avant/après). |
| `schema_version` | Version des migrations. |

Les tables d'identité (`users`, `local_credentials`, `identity_providers`, `allowed_tenants`, `server_config`) ne sont lisibles que par le rôle technique `lattis_service` (utilisé par `lattisd`) et par les propriétaires de serveur. Les autres tables sont lisibles à travers des vues `security_invoker` filtrées par les droits de l'utilisateur.

### 5.2 Un schéma par classeur (`wb_<slug>`)

- **Tableaux** : une table PostgreSQL par tableau, **nommée comme le tableau** (identifiant entre guillemets), pour que le SQL d'un DBA reste lisible. La correspondance avec les identifiants internes de Lattis est dans `lattis_meta_tables` / `lattis_meta_columns`.
- **Colonnes système** de chaque tableau :

| Colonne | Type | Rôle |
|---|---|---|
| `lattis_id` | `uuid` | Identifiant de ligne (UUIDv7, fourni par le client). Clé primaire. |
| `lattis_pos` | `text COLLATE "C"` | Ordre d'affichage partagé (clé fractionnaire, même algorithme que `internal/order` de Lattis). Indexée. |
| `lattis_xid` | `xid8` | Transaction de la dernière modification (déclencheur, `pg_current_xact_id()`). Indexée. |
| `lattis_author` | `text` | Rôle PostgreSQL du créateur de la ligne (déclencheur, `session_user`), non modifiable. |
| `lattis_updated_by` | `text` | Rôle du dernier modificateur (déclencheur). |
| `lattis_updated_at` | `timestamptz` | Date de la dernière modification (déclencheur). |

- **Mise en page et objets** : tables `lattis_meta_*`, miroir des tables `_lattis_*` du format local (feuilles, positions des tableaux, colonnes et types, largeurs et hauteurs, styles, mises en forme conditionnelles, noms, graphiques, TCD, boutons, contrôles, pages, scripts). Chaque ligne porte `lattis_xid` comme les tableaux, pour la synchronisation. Les scripts sont versionnés (`lattis_meta_script_versions`).
- **Propriété** : tous les objets du schéma appartiennent au rôle `NOLOGIN` `wb_<slug>_owner`. `ROW LEVEL SECURITY` est **activée et forcée** (`FORCE`) sur chaque tableau.

### 5.3 Types

| Lattis | PostgreSQL |
|---|---|
| Texte | `text` |
| Entier | `bigint` |
| Décimal | `numeric` |
| Date | `date` (date seule) / `timestamptz` (date et heure) |
| Booléen | `boolean` |

Les types sont **figés à la publication**. Changer un type est une opération de structure (concepteur), faite par `lattis.alter_column_type` sous verrou consultatif.

## 6. Modes de classeur

| | Figé | Collaboratif |
|---|---|---|
| Données, structure, mise en page | Lecture seule pour tous | Selon les rôles (§8) |
| Rôles | Un seul rôle lecteur (+ propriétaires) | Rôles cumulables et restrictions |
| RLS | Non | Oui |
| Mise à jour | L'auteur **republie** une nouvelle version (remplacement atomique du schéma, `version` + 1) | Écritures concurrentes (§10) |
| Côté client | Tri, filtre, calculs personnels en local, rien n'est renvoyé | Synchronisation dans les deux sens |

Republier un classeur figé se fait dans une transaction : création de `wb_<slug>__v<n+1>`, copie, bascule de `workbooks.schema_name`, suppression de l'ancienne version après un délai de grâce (les clients ouverts finissent leur lecture).

## 7. Identification et connexion

### 7.1 Principe

`lattisd` est **l'unique émetteur OIDC** que PostgreSQL connaît. Il fédère les fournisseurs réels :

1. Le client lit `GET /.well-known/lattis.json` (§11) : nom affiché, logo, fournisseurs actifs, points de connexion PostgreSQL.
2. L'utilisateur choisit un fournisseur sur la page de connexion de Lattis.
3. Le client lance le flux **code d'autorisation + PKCE** auprès de `lattisd` (`/oauth/authorize`, navigateur du système, redirection en boucle locale) ; `lattisd` délègue au fournisseur choisi (ou affiche son propre formulaire pour les comptes à mot de passe).
4. Au retour, `lattisd` vérifie le jeton d'identité du fournisseur (signature, `iss`, `aud`, `exp`, tenant autorisé), applique le mode d'accès (§7.4), puis émet **ses propres jetons** : jeton d'accès JWT court (15 min par défaut) et jeton de rafraîchissement (rotation à chaque usage).
5. Le client se connecte à PostgreSQL en `OAUTHBEARER` avec le jeton d'accès `lattisd`. Le module `lattis_oauth` ne connaît qu'un émetteur et une clé publique.

Avantages : toute la logique multi-fournisseurs est en Go dans `lattisd` ; le module C reste minuscule ; `psql` et les autres clients libpq peuvent aussi se connecter (`pg_hba` annonce `lattisd` comme émetteur, flux « device code »).

### 7.2 Fournisseurs et tenants

| Fournisseur | Vérification du tenant |
|---|---|
| Mot de passe Lattis | Compte dans `local_credentials` ; Argon2id ; verrouillage après échecs répétés ; changement et réinitialisation par un propriétaire de serveur. |
| Google | Adresse dans `allowed_tenants` (`email`) ou `any` ; `email_verified` obligatoire. |
| Google Workspace | Revendication `hd` ∈ domaines autorisés. |
| Microsoft (Entra ID) | Revendication `tid` ∈ tenants autorisés (application multi-tenant). |
| Keycloak | `iss` = realm autorisé. |
| OIDC générique | `iss` déclaré, filtre facultatif sur une revendication. |

Les applications OAuth « Lattis » (Google, Microsoft) sont fournies par Be Lattis par défaut ; chaque serveur peut déclarer les siennes (`client_id`, secret dans le gestionnaire de secrets). Pour Keycloak et OIDC générique, le client est déclaré par l'administrateur.

### 7.3 Module `lattis_oauth`

- Chargé par `oauth_validator_libraries = 'lattis_oauth'`.
- `pg_hba.conf` : `hostssl all all 0.0.0.0/0 oauth issuer="https://<lattisd>/" scope="openid lattis" validator=lattis_oauth delegate_ident_mapping=1` (plus des lignes `scram-sha-256` pour les comptes techniques).
- Vérifie : signature (clé publique de `lattisd`, lue dans un fichier, rechargée à la rotation), `iss`, `aud = postgres`, `exp`, `nbf`, et que la revendication `pg_role` est égale au rôle demandé à la connexion. Renvoie l'identité authentifiée (`authn_id` = `lattis:<user_id>`), visible par `system_user`.
- Aucun accès réseau, aucune lecture de table pendant l'authentification **[à vérifier : contraintes exactes de l'API des validateurs]**.

### 7.4 Mode d'accès et rôle par défaut

Réglés dans `server_config` :
- **Sur invitation** : seule une adresse invitée (`invitations`) peut finir la connexion ; le compte PostgreSQL est créé à l'invitation (`CREATE ROLE "<email>" LOGIN`, sans mot de passe) et activé à la première connexion.
- **Entrée libre** : tout utilisateur d'un tenant autorisé ; à la première connexion, `lattisd` crée le compte et l'inscrit dans le **rôle par défaut des arrivants**.
- **Rôle par défaut** : aucun droit, lecteur, participant… au niveau du serveur, surchargeable par fournisseur ou tenant (`allowed_tenants.default_role`) et par classeur.

Création des comptes : `lattisd` se connecte avec `lattis_service`, rôle doté de `CREATEROLE` (délégation PostgreSQL 16+ : il n'administre que les rôles qu'il a créés), jamais superutilisateur.

### 7.5 Comptes techniques

Les DBA et les outils peuvent se connecter en `scram-sha-256` sur des rôles déclarés hors de Lattis. Ils sont soumis aux mêmes droits et à la même RLS (aucun ne doit avoir `BYPASSRLS` sauf décision explicite).

## 8. Autorisations

### 8.1 Rôles fournis

| Rôle | Portée | Droits |
|---|---|---|
| Propriétaire | Classeur | Tout, dont la gestion des droits. Plusieurs propriétaires possibles. |
| Concepteur | Classeur | Tout sauf les droits : structure, mise en page, formules, scripts (publiés et exécutés chez les autres après leur accord). |
| Éditeur | Nœud | Lire, ajouter, modifier, supprimer des lignes. |
| Participant | Nœud | Tout lire ; ajouter des lignes ; modifier et supprimer **les siennes** (`lattis_author`). |
| Lecteur | Nœud | Lire. |

Rôles **personnalisés** : créés par un propriétaire ; un niveau (`none`, `reader`, `participant`, `editor`) par nœud de l'arborescence classeur → feuilles → tableaux, hérité du parent et surchargeable, et un filtre de lecture et un filtre d'écriture facultatifs par tableau. Pas de masquage de colonnes : les droits sont par tableau.

### 8.2 Cumul et restrictions

- Un utilisateur (ou une équipe, dont les membres héritent) reçoit **plusieurs rôles** ; ses droits sont leur **union** (droits PostgreSQL cumulés, règles permissives combinées en OU).
- Les **restrictions** retirent des droits : règles `AS RESTRICTIVE` (combinées en ET), pour tous ou pour certains rôles.

### 8.3 Compilation

La source de vérité est `roles`, `role_grants`, `members`, `restrictions`. La fonction `lattis.compile_acl(workbook)` génère, dans une transaction et de façon idempotente :
- un rôle de groupe `NOLOGIN` par rôle Lattis (`wb_<slug>_r_<nom>`), les appartenances (`GRANT … TO "<utilisateur>"`) ;
- par tableau, le niveau effectif de chaque rôle (résolution de l'héritage) traduit en `GRANT` :

| Niveau | `GRANT` | Règles permissives `TO` le rôle |
|---|---|---|
| Lecteur | `SELECT` | `FOR SELECT USING (<filtre lecture> ou true)` |
| Participant | `SELECT, INSERT, UPDATE, DELETE` (sauf `UPDATE` sur `lattis_author`) | `SELECT USING (true)` ; `INSERT WITH CHECK (lattis_author = current_user)` ; `UPDATE, DELETE USING (lattis_author = current_user)` |
| Éditeur | `SELECT, INSERT, UPDATE, DELETE` | `FOR ALL USING (<filtre lecture> ou true) WITH CHECK (<filtre écriture> ou true)` |

- les restrictions en `AS RESTRICTIVE` ;
- les droits sur `lattis_meta_*` (lecture pour tous les membres, écriture par les fonctions du concepteur seulement).

Les règles générées portent un préfixe `lattis_` ; `lattis.check_drift(workbook)` signale toute règle ou tout droit ajouté ou retiré à la main.

### 8.4 Structure et mise en page

Aucun membre n'a de droit DDL. Les opérations de structure passent par des fonctions `SECURITY DEFINER` (§9.3) qui vérifient l'appartenance au groupe concepteur (`pg_has_role(session_user, …)`), prennent un verrou consultatif par classeur (`pg_advisory_xact_lock`), font le DDL au nom du propriétaire `NOLOGIN` et recompilent les droits.

### 8.5 « Voir en tant que… »

`lattis.preview_as(workbook, user)` (concepteur et propriétaire) : `SET LOCAL ROLE` dans une transaction en lecture seule, renvoie ce que voit l'utilisateur ; journalisé dans `audit`.

### 8.6 Pièges couverts

- `FORCE ROW LEVEL SECURITY` sur toutes les tables, propriétaire `NOLOGIN`.
- Vues (dont les TCD publiés) toujours `security_invoker = true`.
- `NOTIFY` ne transporte jamais de données, seulement un nom de tableau.
- Les colonnes des filtres reçoivent un index (posé par `compile_acl`).
- Documentation : une contrainte `UNIQUE` révèle l'existence d'une ligne invisible ; préférer des clés techniques pour les données sensibles.

## 9. API SQL

Toutes les fonctions sont dans le schéma `lattis`, `SECURITY DEFINER` avec `search_path` fixé, vérifient les droits de `session_user` et écrivent dans `audit`.

### 9.1 Serveur (propriétaires de serveur)

`set_config(key, value)`, `add_provider(kind, label, issuer, client_id, secret_ref, scopes)`, `update_provider(…)`, `remove_provider(id)`, `allow_tenant(provider_id, kind, value, default_role)`, `disallow_tenant(id)`, `create_password_account(email, display_name)`, `reset_password(user_id)`, `disable_user(user_id)`, `enable_user(user_id)`.

### 9.2 Classeurs et droits

`create_workbook(title, mode) → id`, `drop_workbook(id)`, `republish(id)` (figé), `invite(email, workbook_id, roles[])`, `revoke_invitation(id)`, `create_role(workbook_id, name)`, `drop_role(role_id)`, `set_level(role_id, node_kind, node_id, level, read_filter, write_filter)`, `grant_role(role_id, principal)`, `revoke_role(role_id, principal)`, `add_restriction(workbook_id, table_id, command, applies_to[], predicate, label)`, `drop_restriction(id)`, `compile_acl(workbook_id)`, `check_drift(workbook_id)`, « Voir en tant que… » (`POST /api/preview` de `lattisd`, qui appelle `authorize_preview(actor, workbook_id, user_id)` puis lit sous le rôle de l’utilisateur), `my_capabilities(workbook_id) → jsonb` (niveau effectif par tableau, restrictions qui s'appliquent, rôle du classeur), `login_summary() → jsonb`.

### 9.3 Structure (concepteurs)

`create_table(workbook_id, name, columns jsonb) → table_id`, `rename_table`, `drop_table`, `add_column(table_id, name, type, after)`, `rename_column`, `drop_column`, `alter_column_type`, `set_key(table_id, column)`, `create_index`, `drop_index`, `save_meta(workbook_id, kind, payload jsonb)` (mise en page), `publish_script(workbook_id, name, source) → version`.

## 10. Contrat de synchronisation

Le serveur fournit les moyens ; la logique côté poste est spécifiée dans la spec de l'application.

- **Déclencheurs** sur chaque tableau et chaque `lattis_meta_*` : renseignent `lattis_xid`, `lattis_updated_*`, `lattis_author` (à l'insertion), écrivent une pierre tombale à la suppression, puis `NOTIFY "wb_<slug>", '<table_id>'` (une notification par transaction et par table, dédoublonnée).
- **Lecture des deltas** : `lattis.changes_since(workbook_id, table_id, since xid8) → setof (row, deleted bool)`, filtrée par la RLS ; le client mémorise `pg_snapshot_xmin(pg_current_snapshot())` pris au début de sa lecture comme prochain `since` (évite de perdre une transaction validée tard).
- **Écritures** : `INSERT` / `UPDATE … WHERE lattis_id = $1 AND <colonne> IS NOT DISTINCT FROM <valeur vue>` / `DELETE` ordinaires, dans une transaction par étape d'annulation. Zéro ligne touchée = conflit ou refus ; `lattis.why_not(table_id, row_id) → text` distingue « ligne modifiée » (avec auteur, date et valeur actuelle) de « droits insuffisants ».
- **Pierres tombales** : purgées après 30 jours (réglable) ; un client absent plus longtemps refait un chargement complet.

## 11. API HTTP de `lattisd`

| Méthode et chemin | Rôle |
|---|---|
| `GET /.well-known/lattis.json` | Découverte : nom, logo, version, fournisseurs actifs (libellé, type, icône), mode d'accès, points de connexion PostgreSQL (`rw`, `ro[]`), port, mode TLS, édition (`admin`/`web`). Public. |
| `GET /.well-known/openid-configuration`, `GET /jwks.json` | Métadonnées OIDC de `lattisd` (émetteur pour PostgreSQL et les clients). |
| `GET /oauth/authorize`, `POST /oauth/token`, `POST /oauth/device`, `POST /oauth/revoke` | Flux OAuth (PKCE, rafraîchissement, device code). |
| `GET /login/<provider>`, `GET /callback/<provider>` | Délégation aux fournisseurs. |
| `GET /admin/…`, `POST /admin/do/<action>` | Interface d'administration (édition `admin` et `web`). Connexion par le client interne `lattis-admin` (code consommé par `lattisd`, jamais sur `/oauth/token`) ; chaque page s'exécute sur une connexion OAUTHBEARER ouverte sous l'identité de l'administrateur. |
| `POST /api/preview` | « Voir en tant que… » (§8.5) : jeton d'accès en `Bearer`, lecture seule sous le rôle de l'utilisateur visé. |
| `GET /app/…`, `GET /ws` | Application web et son canal WebSocket (édition `web`). |
| `GET /healthz`, `GET /readyz` | Vivacité ; disponibilité (PostgreSQL parent joignable, migrations à jour). |
| `GET /metrics` | Mesures Prometheus (§13). Réseau interne seulement. |

Limitation de débit sur `/oauth/*` et `/login/*` ; en-têtes de sécurité (HSTS, CSP stricte, `frame-ancestors 'none'`) ; cookies `Secure; HttpOnly; SameSite=Lax` pour l'administration et l'application web ; jeton anti-CSRF.

## 12. Réplication et routage

- **Réplication en flux** native ; réplicas en `hot_standby`. La RLS s'applique à l'identique sur les réplicas.
- La découverte annonce un point `rw` (parent) et des points `ro` (réplicas, ou un service unique qui répartit).
- Routage attendu des clients : chargement initial, TCD, requêtes de l'onglet SQL, Power Query et requêtes lourdes → `ro` ; écritures, `LISTEN`, lecture des deltas → `rw`.
- Lecture de ses propres écritures : un client qui vient d'écrire lit sur `rw`, ou attend que `pg_last_wal_replay_lsn()` du réplica dépasse le LSN de sa validation.
- Retard de réplica au-delà d'un seuil (réglable, 10 s par défaut) : `lattisd` retire le réplica de la liste `ro` de la découverte.
- ~~[à vérifier]~~ **Vérifié le 18/10/2026** : `pg_current_snapshot()`, `sync_point()` et `changes_since` fonctionnent sur un réplica (connexion `OAUTHBEARER` comprise) ; les deltas peuvent donc se lire sur `ro`.
- **Constaté** : une session ouverte sur un réplica pendant le rejeu d'une grosse transaction de catalogue (création d'un classeur et de ses tableaux) peut garder des appartenances aux rôles périmées (`pg_has_role` faux alors que `pg_auth_members` est à jour), jusqu'à sa fermeture. Une nouvelle connexion voit juste. Le contrat client impose de rouvrir la connexion sur un refus inattendu venu d'un réplica (contrat §3.1).
- Surveillance par `lattisd` (relevé toutes les 5 s, connexion `lattis_service` à chaque réplica) : retard en secondes (nul quand tout le WAL reçu est rejoué), retard en octets par rapport au parent ; un réplica injoignable, promu ou trop en retard est retiré, puis réannoncé dès qu'il a rattrapé.

## 13. Mesures

Le serveur **n'embarque aucune règle de mise à l'échelle**. `lattisd` expose des mesures Prometheus ; l'exploitant décide (KEDA, HPA, alertes).

| Mesure | Type | Étiquettes |
|---|---|---|
| `lattis_queries_running` | jauge | `server`, `kind` (`heavy`, `light`) |
| `lattis_queries_waiting` | jauge | `server` |
| `lattis_query_duration_seconds` | histogramme | `server`, `kind` (`initial_load`, `pivot`, `sql`, `power_query`, `delta`, `write`) |
| `lattis_replica_lag_seconds`, `lattis_replica_lag_bytes` | jauge | `replica` |
| `lattis_replicas_available` | jauge | |
| `lattis_connections` | jauge | `server`, `state` (`active`, `idle`, `idle_in_transaction`) |
| `lattis_sessions_active` | jauge | `client` (`desktop`, `web`) |
| `lattis_sync_push_total`, `lattis_sync_conflicts_total`, `lattis_sync_delta_rows_total` | compteur | `workbook` |
| `lattis_auth_logins_total` | compteur | `provider`, `result` (`ok`, `denied_tenant`, `not_invited`, `error`) |
| `lattis_enrollments_total` | compteur | `provider`, `mode` |
| `lattis_web_engines_active` | jauge | |
| `lattis_web_engine_memory_bytes`, `lattis_web_cache_disk_bytes` | jauge | |
| `lattis_build_info` | jauge | `version`, `edition`, `pg_version` |

Les mesures « requêtes » sont lues dans `pg_stat_activity` (classement lourd/léger par `application_name` posé par les clients, par exemple `Lattis/initial_load`) et `pg_stat_statements`.

## 14. Version web

- `lattisd` ouvre, pour chaque utilisateur connecté, un **moteur Lattis** : un processus `lattis-engine` (le code Go de l'application, compilé sans Wails, avec le frontend web embarqué) qui écoute en bouclage local, cache SQLite dans `LATTIS_WEB_CACHE_DIR/<user>`. `lattisd` lui relaie `/app/` (frontend, `/app/ws`, envois, téléchargements) après contrôle de la session et de l'origine. Un processus par utilisateur isole les sessions (le moteur garde des états globaux). Environnement minimal : aucun secret de `lattisd` n'est transmis.
- Jetons du moteur : `GET /engine/token` (bouclage local, secret propre au processus en `Authorization: Bearer`) renvoie un jeton d'accès PostgreSQL de 5 minutes de l'utilisateur ; compte désactivé : 403.
- Le moteur se connecte à PostgreSQL **sous l'identité de l'utilisateur** (jeton `lattisd` de l'utilisateur), donc la RLS s'applique comme sur un poste de bureau.
- Canal : WebSocket authentifié (cookie de session), appels JSON-RPC vers les mêmes méthodes que les services Wails, et événements poussés.
- **Comportement identique au bureau**, avec équivalents navigateur : ouvrir et enregistrer = envoi et téléchargement de fichiers ; presse-papiers du navigateur ; secrets des connexions externes chiffrés côté serveur (clé de `lattisd`) au lieu du trousseau ; raccourcis clavier interceptés par la page quand le navigateur le permet.
- **Scripting désactivé par défaut** (Lua, boutons, pages web, serveur MCP). L'administrateur l'active (`server_config.web_scripting = true`) ; même activé, chaque script tourne dans la session de son utilisateur, sans accès au disque du serveur, avec délai, mémoire et réseau limités (liste d'hôtes autorisés).
- Limites par session (mémoire, disque, durée d'inactivité avant fermeture du moteur : 30 min par défaut), nombre maximal de moteurs simultanés.
- Mise à l'échelle horizontale : plusieurs `lattisd` derrière un répartiteur avec affinité de session (le moteur vit dans une instance) ; reprise sur une autre instance par rechargement du cache depuis PostgreSQL.

## 15. Interface d'administration

Écrans (en français, mêmes codes visuels que Lattis) :
1. **Serveur** : nom affiché, logo, mode d'accès, rôle par défaut, scripting web, durées des jetons.
2. **Connexion** : fournisseurs (ajout, ordre, activation, test de connexion) et tenants autorisés avec leur rôle par défaut.
3. **Utilisateurs** : liste, recherche, invitations en attente, désactivation, réinitialisation de mot de passe, dernière connexion. Seul ajout manuel : « Ajouter un utilisateur » crée un compte à mot de passe et un lien pré-signé (7 jours) à transmettre, éventuellement propriétaire du serveur.
4. **Classeurs** : liste, mode, propriétaires, taille, suppression, republication (figé).
5. **Droits d'un classeur** : rôles (fournis et personnalisés), arborescence feuilles → tableaux avec un niveau par nœud, filtres (formulaire simple et SQL), membres, restrictions, « Voir en tant que… », écarts détectés.
6. **Journal** : `audit`, filtrable.
7. **État** : parent, réplicas, retard, connexions, version.

## 16. Configuration de `lattisd`

Seul l'amorçage est hors base (variables d'environnement ou fichier `lattisd.yaml`) ; tout le reste est dans `lattis.server_config`.

| Variable | Rôle |
|---|---|
| `LATTIS_EDITION` | `admin` ou `web`. |
| `LATTIS_PUBLIC_URL` | URL publique (émetteur OIDC). |
| `LATTIS_PG_RW_URL`, `LATTIS_PG_RO_URLS` | Connexions de service (rôle `lattis_service`). |
| `LATTIS_PG_PUBLIC_RW`, `LATTIS_PG_PUBLIC_RO` | Points annoncés aux clients dans la découverte. |
| `LATTIS_SIGNING_KEY_FILE` | Clé privée de signature des jetons (rotation : plusieurs clés acceptées, publiées dans `/jwks.json`, copiées pour `lattis_oauth`). |
| `LATTIS_SECRETS_DIR` ou fournisseur de secrets | Secrets des clients OAuth. |
| `LATTIS_WEB_ENGINE`, `LATTIS_WEB_CACHE_DIR`, `LATTIS_WEB_MAX_ENGINES`, `LATTIS_WEB_IDLE`, `LATTIS_WEB_TOKEN_URL` | Version web. |
| `LATTIS_METRICS_ADDR` | Adresse d'écoute de `/metrics`. |
| `LATTIS_BOOTSTRAP_OWNER` | Adresse du premier propriétaire de serveur (invité au premier démarrage). |

## 17. Déploiement

- **Images** : `be-lattis/server:<version>-pg18` (PostgreSQL 18 officiel + `lattis_oauth` + migrations au démarrage) et `be-lattis/lattisd:<version>`.
- **Docker Compose** : parent, N réplicas, `lattisd`, volume de données, certificats.
- **Kubernetes** (jalon 6, `deploy/helm/lattis`) : chart Helm **autonome** : parent et réplicas en StatefulSets avec l'image `be-lattis/server` (réplication en flux, un emplacement par réplica), `lattisd` (Deployment, Service, Ingress, ServiceMonitor), secrets fournis par l'exploitant. Pas de bascule automatique ni de sauvegardes gérées.
- **Possibilité future, non confirmée** : variante sur l'opérateur **CloudNativePG** (`Cluster`, `instances: 1 + réplicas`, sauvegardes, bascule). Elle demande une image dérivée de celle de CloudNativePG, un amorçage sans le script `initdb` de l'image (pg_hba, TLS et paramètres gérés par l'opérateur) et la clé publique des jetons en secret projeté. Exemple KEDA (`ScaledObject` sur `lattis_queries_waiting`) avec elle. **[à vérifier]** paramètres OAuth acceptés par l'opérateur, sous-ressource `scale` du `Cluster`.
- **Sauvegardes** : `pg_basebackup` + archivage WAL (ou celles de CloudNativePG avec la variante à venir). Restauration testée en CI.
- Guide d'installation et de configuration : `docs/installation.md`.
- **Mises à jour** : migrations SQL versionnées, appliquées par `lattisd --migrate` ; compatibilité ascendante d'une version mineure entre `lattisd` et les clients (la découverte publie la version minimale du client).

## 18. Sécurité

- TLS obligatoire partout (`hostssl` seulement dans `pg_hba`).
- Aucun superutilisateur utilisé en fonctionnement ; `lattis_service` a `CREATEROLE` (délégation PG 16+) et rien d'autre de spécial ; aucun rôle `BYPASSRLS`.
- Jetons d'accès courts, rafraîchissement avec rotation et détection de réutilisation ; révocation par utilisateur (désactivation = `ALTER ROLE … NOLOGIN` + révocation des jetons).
- Secrets OAuth jamais en base ; seulement des références.
- Journal d'audit de toutes les actions d'administration et de toutes les connexions.
- Version web : isolation des moteurs par utilisateur (processus ou au minimum répertoires et connexions séparés), scripting opt-in, CSP stricte.
- Revue de sécurité indépendante avant la première mise en production.

## 19. Exigences non fonctionnelles

- Chargement initial d'un tableau de 100 000 lignes sur réseau local : moins de 3 s.
- Propagation d'une modification vers les autres postes connectés : moins de 1 s au 95e centile (parent et clients sur le même réseau).
- 200 postes simultanés par parent sans pool intermédiaire (PgBouncer en mode transaction incompatible avec `LISTEN` : non pris en charge).
- `lattisd` sans état hors version web (tout l'état est en base).

## 20. Points à vérifier avant réalisation

1. ~~API des validateurs OAuth de PostgreSQL 18~~ **Vérifié le 18/10/2026** (PostgreSQL 18.6) : le validateur reçoit le jeton et le rôle demandé, renvoie `authorized` et `authn_id` ; avec `delegate_ident_mapping=1`, sa décision est finale et `system_user` vaut `oauth:<authn_id>`. Le module lit un fichier et des réglages, sans table ni réseau.
2. ~~Support d'`OAUTHBEARER` par pgx~~ **Vérifié** : pgx v5.11 le gère (`Config.OAuthTokenProvider`).
3. ~~`pg_current_snapshot()` et `changes_since` sur un réplica~~ **Vérifié le 18/10/2026** (§12), avec la réserve du cache des rôles.
4. Sous-ressource `scale` du `Cluster` CloudNativePG (pour l'exemple KEDA).
5. ~~Délégation `CREATEROLE`~~ **Vérifié** : `lattis_service` donne l'appartenance aux groupes qu'il a créés (`lattis_server_owners`) aux rôles qu'il a créés, sans option supplémentaire.
6. Les rôles PostgreSQL sont communs à tout le serveur PostgreSQL : un Serveur Lattis par instance PostgreSQL (une seule base `lattis`).

## 21. Jalons

1. **Socle** : images, migrations du schéma `lattis`, `lattisd` (découverte, OIDC, mot de passe, inscription), module `lattis_oauth`, Docker Compose.
2. **Classeurs figés** : `create_workbook`, publication et republication, rôle lecteur.
3. **Classeurs collaboratifs** : colonnes système, déclencheurs, `changes_since`, `why_not`, rôles fournis, `compile_acl`.
4. **Droits avancés** : rôles personnalisés, filtres, restrictions, « Voir en tant que… », écarts.
5. **Administration web** (édition `admin`) et mesures.
6. **Réplicas** et routage, chart Helm autonome.
7. **Application web** (édition `web`).

Possibilités futures, **non confirmées** (rien de prévu tant que Louis ne les a pas validées) :
- Variante du chart Helm sur l'opérateur **CloudNativePG** : bascule automatique sur un réplica, sauvegardes gérées (§17).
