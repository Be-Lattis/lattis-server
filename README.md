# Serveur Lattis

Serveur préconfiguré qui rend [Lattis](https://github.com/Be-Lattis/lattis-app) collaboratif : PostgreSQL 18 comme source de vérité des classeurs partagés (comptes, droits, sécurité au niveau des lignes), et `lattisd`, un service Go pour la connexion multi-fournisseurs (mot de passe, Google, Google Workspace, Microsoft, Keycloak), l'administration, les mesures et, en option, l'application web.

- Spécification : [docs/specification.md](docs/specification.md)
- Contrat avec l'application de bureau : [docs/contrat-client.md](docs/contrat-client.md)
- **Installer et configurer** (Docker Compose, Kubernetes, variables, réglages) : [docs/installation.md](docs/installation.md)

## État

Jalons 1 à 6 (édition « gestion des rôles ») :

| Fait | Reste |
|---|---|
| Schéma `lattis` (catalogue, journal, jetons) et API SQL du serveur (§9.1) | Flux « device code » pour `psql` |
| `lattisd` : découverte, émetteur OIDC (EdDSA), PKCE, rafraîchissement avec rotation, révocation | Mesures de durée des requêtes et de synchronisation |
| Comptes à mot de passe (Argon2id, verrouillage), Google, Google Workspace, Microsoft, Keycloak, OIDC générique, tenants autorisés | Possibilité future, non confirmée : variante Helm sur CloudNativePG (bascule, sauvegardes) |
| Inscription sur invitation ou entrée libre, premier propriétaire | Application web (jalon 7) |
| Classeurs figés : publication, republication d'un coup, lecteurs | |
| Classeurs collaboratifs : rôles fournis, RLS compilée, deltas, pierres tombales, `why_not`, `NOTIFY` | |
| Droits avancés : rôles personnalisés, niveaux par classeur, feuille ou tableau, filtres de lecture et d'écriture, restrictions, détection des écarts, « Voir en tant que… » (`POST /api/preview`) | |
| Module `lattis_oauth` pour PostgreSQL 18, image `be-lattis/server`, Docker Compose | |
| Administration web `/admin` : serveur, connexion, utilisateurs, classeurs, droits d'un classeur, journal, état | |
| Mesures Prometheus : connexions, requêtes lourdes ou légères, sessions par client, connexions par fournisseur | |
| Réplicas en flux : retard mesuré, retrait de la découverte au-delà du seuil, deltas lisibles sur réplica ; Docker Compose (`--profile replicas`) et chart Helm | |

## Administration web

`http(s)://<serveur>/admin`, réservée aux propriétaires du serveur. La connexion suit le même parcours que l'application (fournisseurs configurés), puis `lattisd` ouvre une connexion PostgreSQL **sous l'identité de l'administrateur** (jeton signé pour lui, OAUTHBEARER) : chaque action passe par les fonctions `lattis.*`, qui vérifient ses droits et journalisent sous son nom. Aucune page n'utilise `lattis_service` pour écrire.

## Essai local

```sh
cd deploy/compose
cp .env.example .env        # mots de passe et adresse du premier propriétaire
docker compose up --build   # --profile replicas : avec un réplica (port 5433)
docker compose logs lattisd # lien « /setup?token=… » pour choisir le mot de passe
```

Puis, dans Lattis, ajouter le serveur `http://localhost:8080`. Le certificat TLS de PostgreSQL est autosigné (`sslmode=require`) : réservé à l'essai. Production et Kubernetes : [docs/installation.md](docs/installation.md).

## Composants

| Chemin | Rôle |
|---|---|
| `cmd/lattisd` | Service ; `lattisd --migrate`, `lattisd migrate`, `lattisd keygen FICHIER`, `lattisd public-keys`, `lattisd init-dev DIR` |
| `schema/` | Migrations SQL (appliquées par `lattisd`, rôle `lattis_service`) et `bootstrap.sql` (superutilisateur, une fois) |
| `oauth/` | Module C `lattis_oauth` (`oauth_validator_libraries`) |
| `deploy/postgres/initdb` | Initialisation de l'image : `lattis_service`, TLS, `pg_hba` (OAuth pour les utilisateurs, mot de passe pour les comptes techniques) |
| `deploy/postgres/replica.sh` | Point d'entrée `lattis-replica` de l'image : copie du parent puis `hot_standby` |
| `docker/`, `deploy/compose` | Images et Docker Compose |
| `deploy/helm/lattis` | Chart Helm (parent, réplicas, `lattisd`) |

### Configuration de `lattisd`

Variables d'environnement et réglages en base : tableau complet dans [docs/installation.md](docs/installation.md#5-variables-denvironnement-de-lattisd).

### Rotation de la clé de signature

1. Ajouter la nouvelle clé **en tête** du fichier `LATTIS_SIGNING_KEY_FILE`, garder l'ancienne derrière (ou sa seule clé publique).
2. `lattisd public-keys > public.pem` et copier ce fichier pour PostgreSQL (`lattis_oauth.public_keys_file`, relu à chaque connexion).
3. Redémarrer `lattisd`. Retirer l'ancienne clé une fois ses jetons expirés (15 min par défaut).

### Exemples d'administration en SQL

```sql
SELECT lattis.set_config('access_mode', '"open"');
SELECT lattis.add_provider('google_workspace', 'Google Acme', NULL, '<client_id>', 'google-acme', NULL);
SELECT lattis.allow_tenant('<provider_id>', 'domain', 'acme.fr', 'reader');
SELECT lattis.invite('marie@acme.fr', NULL, '{server_owner}');
SELECT lattis.create_password_account('paul@acme.fr', 'Paul');  -- renvoie le jeton du lien /setup
```

## Tests

```sh
go test ./...                                          # tests unitaires
docker run -d -p 55432:5432 -e POSTGRES_PASSWORD=pg postgres:18
LATTIS_TEST_PG_URL=postgres://postgres:pg@127.0.0.1:55432/postgres go test ./...
```

Les tests de `internal/e2e` visent un serveur équipé de `lattis_oauth` (variables décrites en tête des fichiers) : connexion `OAUTHBEARER` réelle, jetons refusés, et parcours complet d'un poste contre le Docker Compose.

## Licence

Le Serveur Lattis (`lattisd`, le schéma SQL, le module `lattis_oauth` et les fichiers de déploiement) est un logiciel libre sous **GNU Affero General Public License v3.0** (AGPL-3.0) : voir [LICENSE](LICENSE). Vous pouvez l'utiliser, l'étudier, le modifier et le redistribuer. Si vous proposez une version modifiée à des utilisateurs à travers le réseau, vous devez leur offrir son code source (article 13) : chaque page servie par `lattisd` porte un lien « Code source », à faire pointer vers votre version avec `LATTIS_SOURCE_URL`.

L'application Lattis ([lattis-app](https://github.com/Be-Lattis/lattis-app)) est sous Mozilla Public License 2.0 ; son paquet `protocol`, importé par `lattisd`, l'est aussi, ce qui est compatible avec l'AGPL-3.0.

Dépendances, toutes compatibles avec l'AGPL-3.0 :

| Composant | Licence |
|---|---|
| `github.com/jackc/pgx` et ses modules | MIT |
| `golang.org/x/crypto`, `x/oauth2`, `x/sync`, `x/sys`, `x/text`, `google.golang.org/protobuf` | BSD-3-Clause |
| `github.com/coreos/go-oidc`, `github.com/go-jose/go-jose`, `github.com/prometheus/*` | Apache-2.0 |
| `github.com/cespare/xxhash`, `github.com/beorn7/perks`, `github.com/munnerz/goautoneg`, `github.com/kylelemons/godebug` | MIT, BSD ou Apache-2.0 |
| `github.com/lalamefine/lattis/protocol` | MPL-2.0 |
| Module `lattis_oauth` : PostgreSQL (en-têtes, chargement) | PostgreSQL License |
| Module `lattis_oauth` : OpenSSL 3 (`libcrypto`) | Apache-2.0 |
| Images : PostgreSQL 18 (Debian), distroless | PostgreSQL License, licences Debian, Apache-2.0 |

## Marques

« Lattis », « Be Lattis » et leurs logos sont des marques réservées par leur titulaire. La licence AGPL-3.0 porte sur le code et ne donne aucun droit sur ces noms et logos : une version modifiée distribuée ou proposée en service doit porter un autre nom, sauf accord écrit. Mentionner que votre version est « basée sur le Serveur Lattis » reste permis.
