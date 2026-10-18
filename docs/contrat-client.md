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

## 4. Après connexion

- `SELECT lattis.login_summary()` : utilisateur, propriétaire du serveur ou non, classeurs (vide tant que les classeurs ne sont pas livrés).
- Un propriétaire du serveur administre avec les fonctions `lattis.*` sous sa propre identité (journalisées dans `lattis.audit`).
