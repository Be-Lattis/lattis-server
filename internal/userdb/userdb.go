// Package userdb ouvre des connexions PostgreSQL sous l'identité d'un
// utilisateur, pour que l'interface d'administration applique exactement les
// mêmes droits que l'application de bureau : lattisd signe un jeton d'accès
// court pour l'utilisateur et se connecte avec OAUTHBEARER, comme un poste.
package userdb

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

// Runner exécute fn dans une transaction dont session_user est le rôle de u.
type Runner interface {
	As(ctx context.Context, u store.User, fn func(pgx.Tx) error) error
}

// tokenTTL : durée des jetons signés pour ouvrir une connexion. Une
// connexion ouverte reste valide après expiration du jeton.
const tokenTTL = 5 * time.Minute

// idleClose : un pool sans usage depuis ce délai est fermé.
const idleClose = 10 * time.Minute

// OAuth ouvre un petit pool par utilisateur actif.
type OAuth struct {
	base   *pgxpool.Config
	keys   *tokens.KeySet
	issuer string

	mu    sync.Mutex
	pools map[string]*entry
}

type entry struct {
	pool *pgxpool.Pool
	used time.Time
}

// NewOAuth prépare les connexions à partir de l'URL de lattis_service
// (hôte, base, TLS) ; l'utilisateur et le mot de passe sont remplacés.
func NewOAuth(rwURL string, keys *tokens.KeySet, issuer string) (*OAuth, error) {
	cfg, err := pgxpool.ParseConfig(rwURL)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 2
	cfg.MinConns = 0
	cfg.MaxConnIdleTime = 2 * time.Minute
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.ConnConfig.Password = ""
	cfg.ConnConfig.RuntimeParams["application_name"] = "lattisd/admin"
	return &OAuth{base: cfg, keys: keys, issuer: issuer, pools: map[string]*entry{}}, nil
}

func (o *OAuth) pool(ctx context.Context, u store.User) (*pgxpool.Pool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e, ok := o.pools[u.ID]; ok && e.pool.Config().ConnConfig.User == u.PGRole {
		e.used = time.Now()
		return e.pool, nil
	} else if ok {
		e.pool.Close()
		delete(o.pools, u.ID)
	}
	cfg := o.base.Copy()
	cfg.ConnConfig.User = u.PGRole
	id, role := u.ID, u.PGRole
	cfg.ConnConfig.OAuthTokenProvider = func(context.Context) (string, error) {
		now := time.Now()
		return o.keys.Sign(tokens.TypeAccess, tokens.Claims{
			Issuer: o.issuer, Subject: id, Audience: tokens.Audience{tokens.AudiencePostgres},
			IssuedAt: now.Unix(), NotBefore: now.Unix(), ExpiresAt: now.Add(tokenTTL).Unix(), ID: tokens.NewID(),
			ClientID: "lattisd", Scope: "openid lattis", PGRole: role,
		})
	}
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	o.pools[u.ID] = &entry{pool: p, used: time.Now()}
	return p, nil
}

// As ouvre (ou réutilise) une connexion de l'utilisateur.
func (o *OAuth) As(ctx context.Context, u store.User, fn func(pgx.Tx) error) error {
	p, err := o.pool(ctx, u)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, p, fn)
}

// Forget ferme les connexions d'un utilisateur (déconnexion, désactivation).
func (o *OAuth) Forget(userID string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if e, ok := o.pools[userID]; ok {
		e.pool.Close()
		delete(o.pools, userID)
	}
}

// Sweep ferme les pools inutilisés ; à appeler périodiquement.
func (o *OAuth) Sweep() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, e := range o.pools {
		if time.Since(e.used) > idleClose {
			e.pool.Close()
			delete(o.pools, id)
		}
	}
}

// Close ferme tout.
func (o *OAuth) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id, e := range o.pools {
		e.pool.Close()
		delete(o.pools, id)
	}
}

// SessionAuth emprunte l'identité par SET SESSION AUTHORIZATION depuis une
// connexion de superutilisateur. Réservé aux tests.
type SessionAuth struct{ Pool *pgxpool.Pool }

func (s SessionAuth) As(ctx context.Context, u store.User, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL SESSION AUTHORIZATION "+pgx.Identifier{u.PGRole}.Sanitize()); err != nil {
			return err
		}
		return fn(tx)
	})
}
