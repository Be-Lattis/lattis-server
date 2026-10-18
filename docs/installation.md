# Installer et configurer le Serveur Lattis

Édition « gestion des rôles » (`LATTIS_EDITION=admin`). Trois façons de le lancer : Docker Compose pour un essai, Docker Compose sur un serveur, ou Kubernetes avec le chart Helm. La configuration est la même partout : des variables d'environnement pour `lattisd` et PostgreSQL, le reste en base, modifiable depuis l'administration web.

## 1. Ce qu'il faut savoir avant de commencer

Un Serveur Lattis, c'est trois pièces :

| Pièce | Rôle | Image |
|---|---|---|
| PostgreSQL 18 parent | Source de vérité : comptes, droits, classeurs partagés. Les postes s'y connectent **directement** (port 5432, TLS obligatoire, jeton `OAUTHBEARER`). | `be-lattis/server` (PostgreSQL 18 officiel + module `lattis_oauth`) |
| Réplicas (facultatifs) | Copies en lecture seule pour les lectures lourdes (§12 de la spécification). | même image, point d'entrée `lattis-replica` |
| `lattisd` | Connexion des utilisateurs (mot de passe, Google, Microsoft, Keycloak…), émission des jetons, découverte, administration `/admin`, mesures. | `be-lattis/lattisd` |

Conséquences pratiques :

- **Deux adresses publiques** : celle de `lattisd` (HTTPS, par exemple `https://lattis.acme.fr`) et celle de PostgreSQL (par exemple `pg.acme.fr:5432`), plus une par réplica. Les postes doivent pouvoir joindre les deux.
- **L'URL publique de `lattisd` est l'émetteur des jetons** : PostgreSQL la vérifie dans chaque jeton. Si elle change, il faut la changer aussi côté PostgreSQL (`lattis_oauth.issuer` et `pg_hba.conf`).
- **Mots de passe des comptes techniques** (`lattis_service`, `lattis_replicator`) : ils entrent dans des URL de connexion, donc sans `@`, `/`, `:`, `?`, `#`, `%` ni virgule. Le plus simple : `openssl rand -hex 24`.

## 2. Essai sur un poste (Docker Compose)

```sh
cd deploy/compose
cp .env.example .env            # mots de passe et adresse du premier propriétaire
docker compose up --build       # ajouter --profile replicas pour un réplica sur le port 5433
docker compose logs lattisd | grep token=
```

1. Ouvrir le lien `http://localhost:8080/setup?token=…` affiché dans les journaux et choisir le mot de passe du premier propriétaire (`LATTIS_BOOTSTRAP_OWNER`).
2. Dans Lattis, ajouter le serveur `http://localhost:8080` (menu Compte), puis se connecter avec ce compte.
3. Administration : `http://localhost:8080/admin`.

`init` crée au premier lancement une clé de signature et un certificat TLS **autosigné** (volume `secrets`) : c'est réservé à l'essai (`sslmode=require`, sans vérification du certificat).

Repartir de zéro : `docker compose down -v` (efface les données).

## 3. Serveur de production avec Docker Compose

Partir de `deploy/compose/docker-compose.yml` et changer :

1. **Adresse publique** : `LATTIS_PUBLIC_URL=https://lattis.acme.fr` dans `.env`, avec un mandataire inverse (Caddy, nginx, Traefik) qui termine le HTTPS devant le port 8080. Ajouter `LATTIS_TRUST_PROXY=true` à `lattisd` pour qu'il lise l'adresse du client dans `X-Forwarded-For`.
2. **Certificat de PostgreSQL** : remplacer le service `init` par vos fichiers dans le volume `/etc/lattis` :

   | Fichier | Contenu |
   |---|---|
   | `tls/server.crt`, `tls/server.key` | Certificat de PostgreSQL, signé par une autorité connue des postes, couvrant `pg.acme.fr` (et les noms des réplicas). |
   | `keys/signing.pem` | Clé de signature des jetons : `docker run --rm -v $PWD:/out be-lattis/lattisd keygen /out/signing.pem` (lisible par l'utilisateur 65532). |
   | `keys/public.pem` | Sa clé publique : `docker run --rm -v $PWD:/k -e LATTIS_SIGNING_KEY_FILE=/k/signing.pem be-lattis/lattisd public-keys /k/public.pem`. |
   | `oauth/<nom>` | Un fichier par secret de fournisseur d'identité (voir §6). |

3. **Points annoncés aux postes** : `LATTIS_PG_PUBLIC_RW=pg.acme.fr:5432` et `LATTIS_PG_SSLMODE=verify-full`.
4. **Ports** : publier 5432 (et ceux des réplicas) vers les postes ; **ne pas** publier 9464 (mesures) hors du réseau interne.
5. **Sauvegardes** : `pg_basebackup` et archivage des WAL, ou `pg_dump` quotidien de la base `lattis` (rôles compris : `pg_dumpall --roles-only`).

Le certificat est copié dans les données de PostgreSQL à l'initialisation : pour le renouveler, remplacer `server.crt` et `server.key` dans le dossier de données (`/var/lib/postgresql/18/docker`) puis `SELECT pg_reload_conf();`.

## 4. Kubernetes (chart Helm)

Le chart `deploy/helm/lattis` déploie `lattisd` (Deployment, Service, Ingress facultatif, ServiceMonitor facultatif), le parent PostgreSQL (StatefulSet) et N réplicas (StatefulSet, un emplacement de réplication par pod). Il ne crée **aucun secret** : on les crée avant.

### 4.1 Secrets

```sh
NS=lattis
kubectl create namespace $NS

# Mots de passe des comptes techniques
kubectl -n $NS create secret generic lattis-passwords \
  --from-literal=postgres-password="$(openssl rand -hex 24)" \
  --from-literal=service-password="$(openssl rand -hex 24)" \
  --from-literal=replication-password="$(openssl rand -hex 24)"

# Clé de signature des jetons
docker run --rm -v $PWD:/out be-lattis/lattisd keygen /out/signing.pem
kubectl -n $NS create secret generic lattis-signing --from-file=signing.pem

# Certificat de PostgreSQL (noms publics du parent et des réplicas)
kubectl -n $NS create secret tls lattis-pg-tls --cert=pg.crt --key=pg.key

# Facultatif : secrets des fournisseurs d'identité, une clé par secret
kubectl -n $NS create secret generic lattis-providers --from-literal=google-acme='<client_secret>'
```

### 4.2 Valeurs

```yaml
# lattis-values.yaml
publicURL: https://lattis.acme.fr
lattisd:
  bootstrapOwner: louis@acme.fr
  ingress:
    enabled: true
    className: nginx
    host: lattis.acme.fr
    tls: [{ secretName: lattis-web-tls, hosts: [lattis.acme.fr] }]
secrets:
  existingSecret: lattis-passwords
  signingKeySecret: lattis-signing
  postgresTLSSecret: lattis-pg-tls
  providersSecret: lattis-providers
postgres:
  publicReadWrite: pg.acme.fr:5432
  service:
    type: LoadBalancer           # les postes se connectent directement à PostgreSQL
  persistence: { size: 50Gi }
replicas:
  count: 2
  publicEndpoints: [pg-ro0.acme.fr:5432, pg-ro1.acme.fr:5432]
metrics:
  serviceMonitor: { enabled: true }
```

```sh
helm install lattis deploy/helm/lattis -n lattis -f lattis-values.yaml
kubectl -n lattis logs deploy/lattis-lattisd | grep token=     # lien du premier propriétaire
```

Points à connaître :

- **Postes hors du cluster** : le parent passe par `postgres.service` (souvent `LoadBalancer`). Chaque réplica est annoncé séparément (`replicas.publicEndpoints`, un par pod, dans l'ordre `-replica-0`, `-replica-1`…) : il faut donc une adresse par pod (un `LoadBalancer` par pod, ou un routage TCP par nom). Sans `publicEndpoints`, ce sont les noms DNS internes des pods, valables seulement pour des postes dans le cluster.
- **Ajouter un réplica** : augmenter `replicas.count` (et `publicEndpoints`). Le pod copie le parent au premier démarrage (`pg_basebackup`), ce qui peut être long sur une grosse base ; il n'est annoncé qu'une fois à jour.
- **Retirer un réplica** : réduire `replicas.count`, puis supprimer son emplacement sur le parent, sinon le parent garde ses WAL : `SELECT pg_drop_replication_slot('lattis_replica_2');` (nom = nom du pod, tirets en `_`).
- **Plusieurs `lattisd`** (`lattisd.replicaCount`) : possible, tout l'état est en base ; la limite de débit est comptée par instance.
- **Clé de signature** : les pods PostgreSQL en tirent la clé publique au démarrage. Après une rotation (§8), redémarrer `lattisd` puis les pods PostgreSQL (`kubectl rollout restart statefulset`).
- **Bascule** : le chart ne promeut pas un réplica tout seul. Si le parent est perdu : `kubectl exec` dans un réplica, `pg_ctl promote`, puis faire pointer le parent sur lui (ou restaurer une sauvegarde). Pour une bascule automatique et des sauvegardes gérées, l'intégration à l'opérateur CloudNativePG reste à faire (spécification §17).

Toutes les valeurs, commentées : [`deploy/helm/lattis/values.yaml`](../deploy/helm/lattis/values.yaml).

## 5. Variables d'environnement de `lattisd`

| Variable | Défaut | Rôle |
|---|---|---|
| `LATTIS_EDITION` | `admin` | Édition. `web` n'est pas encore disponible. |
| `LATTIS_PUBLIC_URL` | (obligatoire) | URL publique, émetteur des jetons. |
| `LATTIS_PG_RW_URL` | (obligatoire) | Connexion de service au parent (`lattis_service`), par exemple `postgres://lattis_service:<mdp>@postgres:5432/lattis?sslmode=require`. |
| `LATTIS_PG_RO_URLS` | vide | Connexions de service aux réplicas, séparées par des virgules : `lattisd` mesure leur retard et ne les annonce que s'ils suivent. Même nombre et même ordre que `LATTIS_PG_PUBLIC_RO`. |
| `LATTIS_PG_PUBLIC_RW` | (obligatoire) | `hôte:port` du parent annoncé aux postes. |
| `LATTIS_PG_PUBLIC_RO` | vide | `hôte:port` des réplicas annoncés aux postes. Sans `LATTIS_PG_RO_URLS`, annoncés tels quels (par exemple un répartiteur qui écarte lui-même les retards). |
| `LATTIS_REPLICA_MAX_LAG` | `10s` | Retard au-delà duquel un réplica est retiré de la découverte. |
| `LATTIS_PG_DATABASE` | `lattis` | Base annoncée aux postes. |
| `LATTIS_PG_SSLMODE` | `verify-full` | `sslmode` annoncé aux postes (`require` pour un certificat autosigné d'essai). |
| `LATTIS_SIGNING_KEY_FILE` | (obligatoire) | Clé(s) de signature des jetons, la première signe. |
| `LATTIS_SECRETS_DIR` | vide | Dossier des secrets des fournisseurs (un fichier par référence). |
| `LATTIS_BOOTSTRAP_OWNER` | vide | Premier propriétaire du serveur, invité au premier démarrage (lien `/setup` dans les journaux). |
| `LATTIS_LISTEN_ADDR` | `:8080` | Adresse HTTP. |
| `LATTIS_TLS_CERT_FILE`, `LATTIS_TLS_KEY_FILE` | vide | HTTPS servi par `lattisd` lui-même (sans mandataire). |
| `LATTIS_TRUST_PROXY` | `false` | Lire l'adresse du client dans `X-Forwarded-For`. |
| `LATTIS_METRICS_ADDR` | `:9464` | Adresse de `/metrics` (réseau interne). |
| `LATTIS_RATE_LIMIT` | `120` | Requêtes par minute et par adresse sur `/oauth/*`, `/login/*`, `/setup`. |
| `LATTIS_MIN_CLIENT_VERSION` | `0.0.0` | Version minimale de Lattis acceptée (publiée dans la découverte). |
| `LATTIS_EXTRA_REDIRECT_URIS` | vide | URI de redirection supplémentaires pour l'application de bureau (en plus du bouclage local). |

Lancement : `lattisd --migrate` (applique les migrations puis sert). Autres commandes : `lattisd migrate`, `lattisd keygen FICHIER`, `lattisd public-keys [FICHIER]`, `lattisd init-dev DIR [HÔTE…]`, `lattisd version`.

Image `be-lattis/server` : `POSTGRES_PASSWORD` (superutilisateur, jamais utilisé par `lattisd`), `LATTIS_SERVICE_PASSWORD`, `LATTIS_ISSUER` (= `LATTIS_PUBLIC_URL`), `LATTIS_REPLICATION_PASSWORD` (crée `lattis_replicator` si présent), fichiers dans `/etc/lattis/tls` et `/etc/lattis/keys/public.pem`. Point d'entrée `lattis-replica` : `LATTIS_PRIMARY_HOST`, `LATTIS_PRIMARY_PORT` (5432), `LATTIS_REPLICATION_PASSWORD`, `LATTIS_REPLICATION_SLOT` (nom, ou `auto` = nom d'hôte).

## 6. Réglages du serveur (administration web)

Tout le reste se règle dans `/admin` (propriétaires du serveur), ou en SQL avec `lattis.set_config` sous son propre compte :

| Réglage | Défaut | Rôle |
|---|---|---|
| `display_name`, `logo_url` | « Serveur Lattis », aucun | Nom et logo (https) montrés aux postes. |
| `access_mode` | `invitation` | `invitation` : seuls les invités entrent ; `open` : entrée libre pour les identités autorisées. |
| `default_role` | `none` | Rôle donné aux arrivants en entrée libre. |
| `workbook_creators` | `everyone` | Qui peut publier un classeur : `everyone` ou `owners`. |
| `access_token_ttl`, `refresh_token_ttl` | 900 s, 30 j | Durée des jetons. |
| `invitation_ttl` | 14 j | Validité d'une invitation. |
| `password_max_failures`, `password_lock_seconds` | 5, 900 s | Verrouillage des comptes à mot de passe. |
| `republish_grace` | 600 s | Délai pendant lequel l'ancienne version d'un classeur figé reste lisible après republication. |
| `tombstone_retention` | 30 j | Conservation des suppressions pour la synchronisation. |

**Fournisseurs d'identité** (Administration › Connexion) : mot de passe (intégré), Google, Google Workspace, Microsoft, Keycloak, OIDC générique. Pour chacun : l'ajouter avec l'identifiant client et la **référence** du secret (le nom du fichier dans `LATTIS_SECRETS_DIR` : le secret n'est jamais en base), puis déclarer chez le fournisseur l'URI de redirection `https://lattis.acme.fr/callback/<identifiant>`, où `<identifiant>` est l'identifiant (UUID) que l'ajout a donné au fournisseur (`lattis.add_provider` le renvoie). Les domaines ou tenants autorisés et leur rôle par défaut se règlent au même endroit.

Équivalent en SQL :

```sql
SELECT lattis.set_config('access_mode', '"open"');
SELECT lattis.add_provider('google_workspace', 'Google Acme', NULL, '<client_id>', 'google-acme', NULL);
SELECT lattis.allow_tenant('<provider_id>', 'domain', 'acme.fr', 'reader');
SELECT lattis.invite('marie@acme.fr', NULL, '{server_owner}');
SELECT lattis.create_password_account('paul@acme.fr', 'Paul');  -- renvoie le jeton du lien /setup
```

## 7. Surveiller

- `GET /healthz` (processus vivant) et `GET /readyz` (base joignable).
- Mesures Prometheus sur `LATTIS_METRICS_ADDR` : connexions et requêtes (`lattis_connections`, `lattis_queries_running`…), sessions par client (`lattis_sessions_active`), réplicas (`lattis_replica_lag_seconds`, `lattis_replica_lag_bytes`, `lattis_replicas_available`).
- Administration › État : version du schéma, connexions, réplicas surveillés (retard, raison d'un retrait) et points annoncés.
- Un réplica retiré l'est dans les 5 s (relevé périodique) et revient seul quand il a rattrapé son retard.

## 8. Mettre à jour, faire tourner la clé

- **Mise à jour** : nouvelle image `lattisd`, démarrée avec `--migrate` ; les migrations sont versionnées et appliquées une fois. Mettre à jour l'image `be-lattis/server` quand le module `lattis_oauth` change (noté dans les versions).
- **Rotation de la clé de signature** :
  1. ajouter la nouvelle clé **en tête** de `signing.pem`, garder l'ancienne derrière ;
  2. régénérer `public.pem` (`lattisd public-keys`) pour PostgreSQL (relu à chaque connexion ; avec Helm, redémarrer les pods PostgreSQL) ;
  3. redémarrer `lattisd`, et retirer l'ancienne clé une fois ses jetons expirés (15 min par défaut).
