# Installer et configurer le Serveur Lattis

Édition « gestion des rôles » (`LATTIS_EDITION=admin`, application web désactivée) ; pour servir aussi l'application dans le navigateur, voir en plus §5.1 (quatre cas). Trois façons de le lancer : Docker Compose pour un essai, Docker Compose sur un serveur, ou Kubernetes avec le chart Helm. La configuration est la même partout : des variables d'environnement pour `lattisd` et PostgreSQL, le reste en base, modifiable depuis l'administration web.

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
docker compose up --build       # réplica sur le port 5433 : décommenter les deux lignes LATTIS_PG_*_RO* de .env, puis --profile replicas
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
5. **Connexions** : chaque poste garde une connexion ouverte sur le PostgreSQL parent (synchronisation), sous son propre rôle, donc sans pooler. L'image règle `max_connections` à 400 à la création de la base (`LATTIS_PG_MAX_CONNECTIONS` pour une autre valeur), ce qui tient de l'ordre de 300 postes simultanés en comptant `lattisd` et les moteurs web. Ces réglages sont réécrits à chaque démarrage (`$PGDATA/lattis.conf`) : une base existante les prend au redémarrage avec la nouvelle image, sans commande à la main (voir [mise-a-jour.md](mise-a-jour.md)). Au-delà de quelques centaines de postes, ajouter des réplicas ne suffit pas : voir le jalon 8 de la spécification (§21).
6. **Sauvegardes** : `pg_basebackup` et archivage des WAL, ou `pg_dump` quotidien de la base `lattis` (rôles compris : `pg_dumpall --roles-only`).

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
- **Plusieurs `lattisd`** (`lattisd.replicaCount`, 2 par défaut) : tout l'état est en base ; la limite de débit est comptée par instance. Les mises à jour se font sans interruption (`docs/mise-a-jour.md`, « Sans interruption »). Édition web : affinité par cookie posée pour ingress-nginx et Traefik (k3s) (`lattisd.ingress.stickyWeb`), l'équivalent est à donner pour un autre contrôleur.
- **Clé de signature** : les pods PostgreSQL en tirent la clé publique au démarrage. Après une rotation (§8), redémarrer `lattisd` puis les pods PostgreSQL (`kubectl rollout restart statefulset`).
- **Bascule** : le chart ne promeut pas un réplica tout seul. Si le parent est perdu : `kubectl exec` dans un réplica, `pg_ctl promote`, puis faire pointer le parent sur lui (ou restaurer une sauvegarde). Pour une bascule automatique et des sauvegardes gérées, une intégration à l'opérateur CloudNativePG est une possibilité future, non confirmée (spécification §17).

Toutes les valeurs, commentées : [`deploy/helm/lattis/values.yaml`](../deploy/helm/lattis/values.yaml).

## 5. Variables d'environnement de `lattisd`

| Variable | Défaut | Rôle |
|---|---|---|
| `LATTIS_EDITION` | `admin` | Édition : `admin` (application web désactivée, `/app/` refusée), ou `web` (application dans le navigateur, moteurs lancés selon `LATTIS_WEB_LAUNCHER`). Voir §5.1. |
| `LATTIS_WEB_ENGINE` | vide | Édition web : chemin du binaire `lattis-engine` (dépôt de l'application), lancé une fois par utilisateur connecté. |
| `LATTIS_WEB_CACHE_DIR` | `/var/lib/lattis/web` | Édition web : un répertoire par utilisateur (cache SQLite, fichiers envoyés). |
| `LATTIS_WEB_MAX_ENGINES` | `50` | Édition web : nombre maximal de moteurs, réserve comprise (au-delà : page « Plus de ressources disponibles », 503). |
| `LATTIS_WEB_POOL_SPARE` | `0` | Édition web : moteurs gardés prêts d'avance, en plus de ceux utilisés (§5.1.4). |
| `LATTIS_WEB_DEMO_MAX`, `LATTIS_WEB_DEMO_IDLE`, `LATTIS_WEB_DEMO_LIFETIME` | `0`, `5m`, `1h` | Démo publique (§5.1.5) : démos au plus (0 : sans plafond propre, sous `LATTIS_WEB_MAX_ENGINES` ; la limite de l'administration s'y ajoute), temps sans onglet avant arrêt, durée d'une démo quand le réglage de l'administration manque. Mode `docker` : `LAUNCHER_DEMO_IDLE` côté lanceur. |
| `LATTIS_WEB_IDLE` | `5m` | Édition web : temps sans onglet ouvert avant l'arrêt d'un moteur (sa mémoire est rendue ; la personne qui revient retrouve son classeur après quelques secondes de démarrage). |
| `LATTIS_SMTP_HOST` | vide | Envoi d'e-mails (facultatif, §5.2) : serveur SMTP. Vide : pas d'envoi, les liens restent à copier depuis l'administration. |
| `LATTIS_SMTP_PORT` | `587`, `465` (tls), `25` (none) | Port du serveur SMTP. |
| `LATTIS_SMTP_TLS` | `starttls` | `starttls`, `tls` (TLS dès la connexion) ou `none` (relais local de confiance seulement). |
| `LATTIS_SMTP_USER`, `LATTIS_SMTP_PASSWORD_FILE` | vide | Identifiant SMTP et fichier contenant son mot de passe. |
| `LATTIS_SMTP_FROM` | vide | Expéditeur (`Lattis <lattis@acme.fr>`), obligatoire avec `LATTIS_SMTP_HOST`. |
| `LATTIS_MAIL_PER_HOUR`, `LATTIS_MAIL_PER_ADDRESS` | `200`, `5` | Plafond d'envoi : e-mails par heure pour tout le serveur, et par destinataire. Au-delà, les messages attendent (24 h au plus). |
| `LATTIS_WEB_LAUNCHER` | `process` | Édition web : lancement des moteurs, `process` (processus de `lattisd`), `docker` (conteneurs par le mini lanceur `lattis-launcher`) ou `kubernetes` (pods). Voir §5.1. |
| `LATTIS_WEB_TOKEN_URL` | vide | Modes `docker` et `kubernetes` : adresse de `/engine/token` vue des moteurs (écoute interne `LATTIS_WEB_ENGINE_LISTEN`, `:8081` par défaut, à ne pas exposer). |
| `LATTIS_WEB_LAUNCHER_URL`, `LATTIS_WEB_LAUNCHER_TOKEN_FILE` | vide | Mode `docker` : adresse du mini lanceur et fichier du jeton partagé avec lui. |
| `LATTIS_WEB_K8S_IMAGE`, `_CPU`, `_MEMORY`, `_CACHE_SIZE`, `_STORAGE_CLASS`, `_PULL_SECRET`, `_NAMESPACE` | `250m`, `1Gi` | Mode `kubernetes` : image de `lattis-engine`, requêtes (la mémoire est aussi la limite), volume de cache par utilisateur (vide : `emptyDir`). Posés par le chart Helm (`web.*`). |
| `LATTIS_WEB_ENGINE_UIDS` | vide | Édition web, Linux : plage d'UID système (`200000-200999`, au moins `LATTIS_WEB_MAX_ENGINES` valeurs) ; chaque moteur tourne sous son propre UID et groupe, sans accès à la clé de signature, aux secrets ni aux caches des autres. Vide : 200000 et suivants si `lattisd` tourne en root, sinon même utilisateur que `lattisd`. Voir §5.1. |
| `LATTIS_WEB_TOKEN_URL` | `http://127.0.0.1:<port>/engine/token` | Édition web : adresse où les moteurs demandent leurs jetons (à préciser si `lattisd` sert lui-même le TLS). |
| `LATTIS_PUBLIC_URL` | (obligatoire) | URL publique, émetteur des jetons. |
| `LATTIS_PG_RW_URL` | (obligatoire) | Connexion de service au parent (`lattis_service`), par exemple `postgres://lattis_service:<mdp>@postgres:5432/lattis?sslmode=require`. |
| `LATTIS_PG_RO_URLS` | vide | Connexions de service aux réplicas, séparées par des virgules : `lattisd` mesure leur retard et ne les annonce que s'ils suivent. Même nombre et même ordre que `LATTIS_PG_PUBLIC_RO`. |
| `LATTIS_PG_PUBLIC_RW` | (obligatoire) | `hôte:port` du parent annoncé aux postes. |
| `LATTIS_PG_PUBLIC_RO` | vide | `hôte:port` des réplicas annoncés aux postes. Sans `LATTIS_PG_RO_URLS`, annoncés tels quels (par exemple un répartiteur qui écarte lui-même les retards). |
| `LATTIS_REPLICA_MAX_LAG` | `10s` | Retard au-delà duquel un réplica est retiré de la découverte. |
| `LATTIS_PG_DATABASE` | `lattis` | Base annoncée aux postes. |
| `LATTIS_PG_SSLMODE` | `verify-full` | `sslmode` annoncé aux postes (`require` pour un certificat autosigné d'essai). |
| `LATTIS_SIGNING_KEY_FILE` | (obligatoire) | Clé(s) de signature des jetons, la première signe. |
| `LATTIS_SECRETS_DIR` | vide | Facultatif : dossier de secrets des fournisseurs (un fichier par référence), à la place d'une valeur saisie dans l'administration. |
| `LATTIS_BOOTSTRAP_OWNER` | vide | Premier propriétaire du serveur, invité au premier démarrage (lien `/setup` dans les journaux). |
| `LATTIS_LISTEN_ADDR` | `:8080` | Adresse HTTP. |
| `LATTIS_TLS_CERT_FILE`, `LATTIS_TLS_KEY_FILE` | vide | HTTPS servi par `lattisd` lui-même (sans mandataire). |
| `LATTIS_TRUST_PROXY` | `false` | Lire l'adresse du client dans `X-Forwarded-For`. |
| `LATTIS_METRICS_ADDR` | `:9464` | Adresse de `/metrics` (réseau interne). |
| `LATTIS_SHUTDOWN_DELAY` | `0s` | À l'arrêt, temps pendant lequel `lattisd` sert encore après être sorti du service (`/readyz` en 503) ; 5 s dans le chart (`docs/mise-a-jour.md`). |
| `LATTIS_SHUTDOWN_GRACE` | `15s` | À l'arrêt, temps laissé aux requêtes en cours. |
| `LATTIS_RATE_LIMIT` | `120` | Requêtes par minute et par adresse sur `/oauth/*`, `/login/*`, `/setup`. |
| `LATTIS_MIN_CLIENT_VERSION` | `0.0.0` | Version minimale de Lattis acceptée (publiée dans la découverte). |
| `LATTIS_EXTRA_REDIRECT_URIS` | vide | URI de redirection supplémentaires pour l'application de bureau (en plus du bouclage local). |
| `LATTIS_DOWNLOAD_URL` | vide | Page de téléchargement de l'application, proposée sur les liens vers un classeur (`/w/<id>`). |
| `LATTIS_SOURCE_URL` | `https://github.com/Be-Lattis/lattis-server` | Lien « Code source » au pied des pages (AGPL-3.0, article 13) : à faire pointer vers le code de votre version si vous l'avez modifiée. |
| `LATTIS_PIPELINE_DIR` | `/var/lib/lattis/pipelines` | Fichiers des pipelines d'ingestion : `<pipeline>/uploads` (envois depuis /admin), `<pipeline>/inbox` (dossier de dépôt), puis `done/` et `failed/`. Volume en écriture pour l'utilisateur de lattisd. |
| `LATTIS_PIPELINE_SECRETS_DIR` | vide | Secrets des pipelines (mot de passe SFTP, jeton d'API, chaîne de connexion), un fichier par secret. Distinct du dossier des clients OAuth : un pipeline n'y a pas accès. Facultatif : un secret saisi dans l'administration est gardé chiffré en base. |
| `LATTIS_PIPELINE_ALLOWED_HOSTS` | vide | Hôtes, `*.domaine` ou réseaux CIDR internes que les pipelines et leurs alertes peuvent joindre. Sans eux, bouclage, réseaux privés, link-local et métadonnées du nuage sont refusés. |
| `LATTIS_TIMEZONE` | `Europe/Paris` | Fuseau des horaires de pipelines, des dates importées sans fuseau et des dates affichées dans l'administration. |

Lancement : `lattisd --migrate` (applique les migrations puis sert). Autres commandes : `lattisd migrate`, `lattisd keygen FICHIER`, `lattisd public-keys [FICHIER]`, `lattisd init-dev DIR [HÔTE…]`, `lattisd version`.

Image `be-lattis/server` : `POSTGRES_PASSWORD` (superutilisateur, jamais utilisé par `lattisd`), `LATTIS_SERVICE_PASSWORD`, `LATTIS_ISSUER` (= `LATTIS_PUBLIC_URL`), `LATTIS_REPLICATION_PASSWORD` (crée `lattis_replicator` si présent), fichiers dans `/etc/lattis/tls` et `/etc/lattis/keys/public.pem`. Point d'entrée `lattis-replica` : `LATTIS_PRIMARY_HOST`, `LATTIS_PRIMARY_PORT` (5432), `LATTIS_REPLICATION_PASSWORD`, `LATTIS_REPLICATION_SLOT` (nom, ou `auto` = nom d'hôte).

### 5.1 Application web : quatre cas

Un Serveur Lattis peut aussi servir l'application dans le navigateur (`/app/`). Chaque utilisateur connecté y reçoit alors son propre moteur `lattis-engine`, arrêté après `LATTIS_WEB_IDLE` d'inactivité. Quatre cas, selon `LATTIS_EDITION` et `LATTIS_WEB_LAUNCHER` :

| | Désactivée (défaut) | `process` | `docker` | `kubernetes` |
|---|---|---|---|---|
| Réglage | `LATTIS_EDITION=admin` | `LATTIS_EDITION=web` (`LATTIS_WEB_LAUNCHER=process`, défaut) | `LATTIS_EDITION=web`, `LATTIS_WEB_LAUNCHER=docker` | Chart : `web.enabled: true` |
| Pour qui | Les utilisateurs travaillent avec l'application de bureau seulement | Un serveur unique, l'installation la plus simple | Un serveur Docker, isolation forte sans root pour `lattisd` | Un cluster |
| `/app/` | Refusée : page « L'application web n'est pas activée sur ce serveur » (404) ; la découverte annonce `"edition": "admin"` | Servie | Servie | Servie |
| Où tourne un moteur | Aucun moteur | Processus de `lattisd`, dans son conteneur | Conteneur à part, un par utilisateur | Pod à part, un par utilisateur |
| Isolation | Sans objet | Avec `lattisd` en root : un utilisateur système par moteur. Sans root : aucune (déconseillé à plusieurs utilisateurs) | Conteneur verrouillé, mémoire, processeurs et processus limités | Pod verrouillé, ressources limitées, NetworkPolicy |
| Ce qu'il faut en plus | Rien (image `lattisd`) | L'image `lattisd:web` (qui contient le moteur) | Le mini lanceur `lattis-launcher`, l'image du moteur | L'image du moteur |
| Démarrage d'un moteur | | Immédiat | Une à deux secondes | Quelques secondes (tirage de l'image la première fois) 
| Réserve de moteurs prêts (§5.1.4) | | Oui | Oui | Oui |

Les quatre cas sont vérifiés par la CI (désactivée, `process` avec et sans isolation, `docker` de bout en bout ; `kubernetes` : génération du chart seulement).

#### 5.1.0 Désactivée

C'est l'installation par défaut, celle des §2 à §4 : rien à faire. Les classeurs s'ouvrent dans l'application de bureau ; une adresse `/app/…` (lien partagé par erreur, ancien favori) affiche que l'application web n'est pas activée. Pour l'activer plus tard, choisir l'un des trois cas suivants : les données ne changent pas.

Images utilisées :

| Image | Construction |
|---|---|
| `lattis-engine` (moteur, dépôt de l'application) | `ghcr.io/be-lattis/lattis-engine:main`, ou `docker build -f build/engine/Dockerfile -t be-lattis/lattis-engine:dev .` dans le dépôt `lattis-app` |
| `lattisd:web` (`lattisd` avec le moteur, mode `process`) | `docker build -f docker/lattisd/Dockerfile --target web --build-arg ENGINE_IMAGE=<image du moteur> -t be-lattis/lattisd:web .` |
| `lattis-launcher` (mode `docker`) | `docker build -f docker/launcher/Dockerfile -t be-lattis/lattis-launcher:dev .` |

Dans les trois cas avec application web, `LATTIS_PUBLIC_URL` est aussi l'adresse que les moteurs contactent pour se connecter à PostgreSQL au nom de l'utilisateur.

#### 5.1.1 Mode `process`

1. Construire `be-lattis/lattisd:web` (tableau ci-dessus).
2. Dans `deploy/compose/.env` :

   ```sh
   LATTIS_IMAGE=be-lattis/lattisd:web
   LATTIS_EDITION=web
   ```

3. `docker compose up -d`, puis ouvrir `http://localhost:8080/app/`.

Pour isoler les moteurs entre eux et de `lattisd` (recommandé dès qu'il y a plusieurs utilisateurs), lancer `lattisd` en root avec seulement six capacités. Chaque moteur reçoit alors un utilisateur système de la plage 200000 et suivantes (`LATTIS_WEB_ENGINE_UIDS` pour en choisir une autre) et ne voit que son propre cache. Fichier `deploy/compose/docker-compose.override.yml`, lu automatiquement par Compose :

```yaml
services:
  lattisd:
    user: "0"
    cap_drop: [ALL]
    cap_add: [SETUID, SETGID, CHOWN, KILL, DAC_OVERRIDE, FOWNER]
```

`SETUID` et `SETGID` servent à lancer chaque moteur sous son utilisateur, `CHOWN` et `FOWNER` à lui donner son répertoire de cache, `KILL` à l'arrêter, `DAC_OVERRIDE` à garder l'accès aux volumes créés pour l'utilisateur 65532. La clé de signature et les secrets restent en `0600` : les moteurs ne les lisent pas. Le chart Helm ne propose pas ce mode : en Kubernetes, utiliser `kubernetes`.

#### 5.1.2 Mode `docker`

Les moteurs tournent chacun dans leur conteneur. Le socket Docker donne root sur la machine : seul le mini lanceur `lattis-launcher` le reçoit, et il ne sait que démarrer, surveiller et arrêter l'image du moteur. `lattisd` lui parle avec un jeton partagé, que le service `init` crée au premier lancement (`/etc/lattis/launcher/token`).

1. Construire `be-lattis/lattis-launcher:dev` et disposer de l'image du moteur (tableau ci-dessus). `lattisd` n'a pas besoin de l'image `web` dans ce mode.
2. Dans `deploy/compose/.env`, décommenter le bloc « moteurs en conteneurs » et régler les adresses :

   ```sh
   LATTIS_EDITION=web
   LATTIS_WEB_LAUNCHER=docker
   LATTIS_WEB_LAUNCHER_URL=http://launcher:8090
   LATTIS_WEB_LAUNCHER_TOKEN_FILE=/etc/lattis/launcher/token
   LATTIS_WEB_TOKEN_URL=http://lattisd:8081/engine/token
   LATTIS_ENGINE_IMAGE=ghcr.io/be-lattis/lattis-engine:main
   # Joignables depuis un conteneur : pas « localhost ».
   LATTIS_PUBLIC_URL=https://lattis.acme.fr
   LATTIS_PG_PUBLIC_RW=pg.acme.fr:5432
   ```

3. `docker compose --profile docker-engines up -d`.
4. Vérifier : après une connexion à `/app/`, `docker ps --filter label=lattis.engine=1` montre le conteneur `lattis-engine-<utilisateur>`.

Les moteurs rejoignent le réseau du projet (`lattis_default`) et y joignent `lattisd` sur le port interne 8081, qui n'est pas publié. Ne publiez pas non plus le port 8090 du lanceur. Limites par moteur : `LAUNCHER_MEMORY` (1g), `LAUNCHER_CPUS` (1), `LAUNCHER_PIDS` (256), à ajouter au service `launcher`. Chaque utilisateur garde un volume de cache `lattis-web-<utilisateur>`.

Pour un essai sur un poste, `localhost` ne convient pas : les moteurs le prendraient pour leur propre conteneur. Utilisez l'adresse IP du poste sur le réseau local (`LATTIS_PUBLIC_URL=http://192.168.1.10:8080`, `LATTIS_PG_PUBLIC_RW=192.168.1.10:5432`).

#### 5.1.3 Mode `kubernetes`

`lattisd` crée un pod par utilisateur dans son espace de noms, au nom d'un compte de service limité aux pods et aux volumes de cet espace. Chaque pod est rattaché au pod de `lattisd` et disparaît avec lui.

1. Ajouter aux valeurs du chart (§4.2) :

   ```yaml
   web:
     enabled: true
     engineImage:
       repository: ghcr.io/be-lattis/lattis-engine
       tag: main
     maxEngines: 50
     resources: { cpu: 250m, memory: 1Gi }
     cache:
       size: 1Gi        # vide : cache perdu à l'arrêt du moteur, rechargé depuis le serveur
       storageClass: ""
   ```

2. `helm upgrade --install lattis deploy/helm/lattis -n lattis -f lattis-values.yaml`.
3. Vérifier : après une connexion à `https://lattis.acme.fr/app/`, `kubectl -n lattis get pods -l app.kubernetes.io/name=lattis-engine`.

Le chart crée le compte de service, ses droits et deux NetworkPolicy : seul `lattisd` joint les moteurs, et seuls les moteurs joignent le port des jetons de `lattisd`. Elles ne s'appliquent qu'avec un réseau de cluster qui les gère (Calico, Cilium…). Les moteurs doivent pouvoir joindre `publicURL` et `postgres.publicReadWrite` depuis le cluster. Ces NetworkPolicy ne filtrent que les entrées : la sortie des moteurs n'est pas restreinte par le chart. Les moteurs téléchargent eux-mêmes les tuiles des cartes (`tile.openstreetmap.org:443`) : si le cluster bloque la sortie par défaut, autorisez pour les pods `app.kubernetes.io/name=lattis-engine` le DNS, `tile.openstreetmap.org:443`, le port 8081 des pods `lattisd`, `publicURL` et PostgreSQL (les hôtes internes restent refusés par le moteur lui-même, hors `--internal-hosts`). Le secret de chaque moteur est passé par une variable d'environnement de son pod : il est lisible par qui peut lire les pods de l'espace de noms.

#### 5.1.4 Réserve de moteurs prêts (les trois modes)

Par défaut, un moteur démarre à la connexion de l'utilisateur. Avec `LATTIS_WEB_POOL_SPARE=N` (chart : `web.spare`), le serveur garde en plus N moteurs démarrés d'avance, prêts à recevoir quelqu'un : avec 15 personnes connectées et N = 2, 17 moteurs tournent. À la connexion, l'utilisateur reçoit un moteur prêt, sans attente, et un neuf démarre pour reformer la réserve.

- **Un moteur ne sert qu'une fois** : attribué à une personne, il disparaît à la fin de sa session (inactivité `LATTIS_WEB_IDLE`, arrêt) et n'est jamais rendu à la réserve. Rien ne passe d'un utilisateur à l'autre.
- **Plafond** : moteurs attribués et moteurs prêts restent ensemble sous `LATTIS_WEB_MAX_ENGINES` (chart : `web.maxEngines`). Quand il est atteint, la réserve ne se reforme plus, et une nouvelle connexion reçoit la page « Plus de ressources disponibles » (503), qui invite à réessayer ou à utiliser l'application de bureau. `LATTIS_WEB_POOL_SPARE` doit rester inférieur au plafond.
- **Cache** : un moteur prêt ne sait pas encore pour qui il travaillera ; son répertoire de travail est éphémère, effacé avec lui. Sans réserve, chaque utilisateur garde son cache d'une session à l'autre ; avec, il le recharge depuis le serveur à chaque session.
- **Ressources** : les moteurs prêts consomment de la mémoire en permanence (en mode `docker` et `kubernetes`, chacun selon ses limites). Deux à cinq suffisent en général à absorber les connexions du matin.

Le moteur ne demande les jetons de l'utilisateur qu'à sa première connexion à PostgreSQL : `lattisd` rattache le secret du moteur prêt à l'utilisateur au moment de l'attribution. Aucune version particulière de `lattis-engine` n'est requise.

#### 5.1.5 Démo publique (les trois modes)

Réglage **Démo publique, sans connexion** (Administration › Serveur › Accès et publication), désactivé par défaut. Une fois coché, l'adresse `/demo` ouvre Lattis dans le navigateur sans compte : on essaie l'outil sans rien installer, avec les données d'exemple « small » ou « medium » (Fichier › Ouvrir › Récents). Rien n'est enregistré sur le serveur : Ctrl+S télécharge le classeur.

- Chaque navigateur a son moteur, lancé avec `--public-demo` : ni serveur, ni jeton, ni secret, donc aucun accès à PostgreSQL ni aux classeurs du serveur ; scripts coupés ; aucun volume persistant (répertoire, volume anonyme ou `emptyDir` effacé à l'arrêt).
- Plafonds : aucun par défaut, sinon celui de l'installation (`LATTIS_WEB_MAX_ENGINES`, partagé avec les sessions) ; la limite « Sessions de démonstration » de l'administration (ou `LATTIS_WEB_DEMO_MAX`) en ajoute un. Un plafond atteint affiche une page qui le nomme. Une démo dure **Durée d'une démo** (Administration › Serveur, 10 min par défaut) : la personne est prévenue 2 minutes avant la fin pour télécharger son classeur, puis la démo s'arrête et son navigateur voit « Démo terminée », avec un lien pour en commencer une autre. Elle s'arrête aussi après `LATTIS_WEB_DEMO_IDLE` sans onglet (5 min). Le moteur reçoit l'échéance par `LATTIS_DEMO_DEADLINE` et `LATTIS_DEMO_WARN_BEFORE`. Chaque navigateur garde sa démo (cookie) ; pas de plafond par adresse IP, qu'un proxy peut rendre commune à tous.
- Demande un `lattis-engine` qui connaît `--public-demo`.

### 5.2 Envoi d'e-mails (facultatif)

Sans serveur d'envoi, rien ne change : les liens (choix du mot de passe, réinitialisation) s'affichent dans l'administration pour être transmis à la main. Avec `LATTIS_SMTP_HOST` et `LATTIS_SMTP_FROM`, le serveur envoie lui-même :

- le **lien de choix du mot de passe** d'un compte créé ou réinitialisé dans l'administration (le lien reste affiché, au besoin) ;
- le **lien « Mot de passe oublié ? »** de la page de connexion : la personne saisit son adresse et reçoit un lien valable 7 jours ; son mot de passe actuel reste valable tant qu'elle ne l'a pas utilisé. La page répond la même chose que l'adresse ait un compte ou non ;
- la **confirmation d'adresse** : quand une personne choisit une autre adresse que celle prévue, un lien de confirmation lui est envoyé ; les invitations adressées à cette adresse ne lui sont ouvertes qu'après confirmation ;
- les **invitations à un classeur** (lien `/w/<id>`) ;
- les **alertes d'échec des pipelines**, aux adresses réglées sur chaque pipeline (page Pipelines).

Les messages passent par une file en base (`lattis.mail_outbox`), servie par `lattisd` sous le plafond d'envoi (`LATTIS_MAIL_PER_HOUR`, `LATTIS_MAIL_PER_ADDRESS`), avec 5 essais au plus. Le contenu d'un message (lien, titre) est effacé une fois envoyé ; il ne reste qu'un journal (type, destinataire, dates, état), visible dans la page **État** de l'administration, qui a aussi un bouton **Envoyer un e-mail de test**.

Compose : ajoutez les variables au service `lattisd` et le mot de passe dans un fichier du volume des secrets (par exemple `/etc/lattis/smtp-password`). Chart Helm : `lattisd.extraEnv`, et le mot de passe dans un secret monté.

## 6. Réglages du serveur (administration web)

Tout le reste se règle dans `/admin` (propriétaires du serveur), ou en SQL avec `lattis.set_config` sous son propre compte :

| Réglage | Défaut | Rôle |
|---|---|---|
| `display_name`, `logo_url` | « Serveur Lattis », aucun | Nom et logo (https) montrés aux postes. |
| `access_mode` | `invitation` | `invitation` : seuls les invités entrent ; `open` : entrée libre pour les identités autorisées. |
| `default_role` | `none` | Rôle donné aux arrivants en entrée libre. |
| `workbook_creators` | `everyone` | Qui peut publier un classeur : `everyone`, ou `owners` (propriétaires du serveur et personnes qui ont le rôle « créateur de classeurs », donné depuis la page Utilisateurs de l'administration ou par `lattis.set_workbook_creator(user_id, true)`). |
| `access_token_ttl`, `refresh_token_ttl` | 900 s, 30 j | Durée des jetons. |
| `invitation_ttl` | 14 j | Validité d'une invitation. |
| `password_max_failures`, `password_lock_seconds` | 5, 900 s | Verrouillage des comptes à mot de passe. |
| `republish_grace` | 600 s | Délai pendant lequel l'ancienne version d'un classeur figé reste lisible après republication. |
| `tombstone_retention` | 30 j | Conservation des suppressions pour la synchronisation. |

**Fournisseurs d'identité** (Administration › Connexion) : mot de passe (intégré), Google, Google Workspace, Microsoft, Keycloak, OIDC générique. Pour chacun : l'ajouter avec l'identifiant client et son **secret**. Le secret est saisi en valeur : `lattisd` le garde chiffré en base (AES-256-GCM, clé tirée de sa clé de signature), et l'administration ne l'affiche plus jamais, elle permet seulement de le remplacer. On peut encore donner à la place le nom d'un fichier de `LATTIS_SECRETS_DIR`. Les secrets saisis suivent une rotation de la clé de signature tant que l'ancienne clé reste dans le fichier ; une fois l'ancienne clé retirée, ceux qu'elle chiffrait sont à saisir de nouveau. puis déclarer chez le fournisseur l'URI de redirection `https://lattis.acme.fr/callback/<identifiant>`, où `<identifiant>` est l'identifiant (UUID) que l'ajout a donné au fournisseur (`lattis.add_provider` le renvoie). Les domaines ou tenants autorisés et leur rôle par défaut se règlent au même endroit.

Équivalent en SQL :

```sql
SELECT lattis.set_config('access_mode', '"open"');
SELECT lattis.add_provider('google_workspace', 'Google Acme', NULL, '<client_id>', 'google-acme', NULL);
SELECT lattis.allow_tenant('<provider_id>', 'domain', 'acme.fr', 'reader');
SELECT lattis.invite('marie@acme.fr', NULL, '{server_owner}');
SELECT lattis.create_password_account('paul@acme.fr', 'Paul');  -- renvoie le jeton du lien /setup
```

## 7. Surveiller

Comment le serveur tient la charge (bureau, web), ses goulets et les limites réglables dans l'administration : [montee-en-charge.md](montee-en-charge.md).

- `GET /healthz` (processus vivant) et `GET /readyz` (base joignable).
- Mesures Prometheus sur `LATTIS_METRICS_ADDR` : connexions et requêtes (`lattis_connections`, `lattis_queries_running`…), sessions par client (`lattis_sessions_active`), réplicas (`lattis_replica_lag_seconds`, `lattis_replica_lag_bytes`, `lattis_replicas_available`).
- Administration › État : version du schéma, connexions, réplicas surveillés (retard, raison d'un retrait) et points annoncés.
- Administration › Surveillance : utilisateurs connectés face aux moteurs web (processus, conteneurs ou pods, réserve comprise) et mémoire des moteurs et de lattisd, en chiffres et en petits graphiques de la dernière heure (relevé toutes les 30 s, gardé en mémoire par chaque lattisd ; la page se rafraîchit seule). Mémoire des moteurs : `/proc` en mode `process` (Linux), statistiques Docker relevées par `lattis-launcher` en mode `docker`, metrics-server en mode `kubernetes` (le chart donne le droit de lecture de `pods.metrics.k8s.io` ; sans metrics-server, la page le signale).
- Un réplica retiré l'est dans les 5 s (relevé périodique) et revient seul quand il a rattrapé son retard.

## 8. Mettre à jour, faire tourner la clé

Mises à jour : voir [mise-a-jour.md](mise-a-jour.md) (migrations automatiques, essai à blanc, sauvegarde, retour en arrière).

- **Mise à jour** : nouvelle image `lattisd`, démarrée avec `--migrate` ; les migrations sont versionnées et appliquées une fois. Mettre à jour l'image `be-lattis/server` quand le module `lattis_oauth` change (noté dans les versions).
- **Rotation de la clé de signature** :
  1. ajouter la nouvelle clé **en tête** de `signing.pem`, garder l'ancienne derrière ;
  2. régénérer `public.pem` (`lattisd public-keys`) pour PostgreSQL (relu à chaque connexion ; avec Helm, redémarrer les pods PostgreSQL) ;
  3. redémarrer `lattisd`, et retirer l'ancienne clé une fois ses jetons expirés (15 min par défaut).
