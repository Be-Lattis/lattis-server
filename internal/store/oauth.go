package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Code est un code d'autorisation en attente d'échange.
type Code struct {
	UserID        string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Scope         string
	Nonce         string
}

// SaveCode enregistre un code d'autorisation (seul son hachage est gardé).
func (s *Store) SaveCode(ctx context.Context, code string, c Code, ttl time.Duration) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO lattis.oauth_codes
		(code_hash, user_id, client_id, redirect_uri, code_challenge, scope, nonce, expires_at)
		VALUES ($1, $2::uuid, $3, $4, $5, $6, nullif($7, ''), now() + make_interval(secs => $8))`,
		Hash(code), c.UserID, c.ClientID, c.RedirectURI, c.CodeChallenge, c.Scope, c.Nonce, ttl.Seconds())
	return err
}

// ConsumeCode échange un code une seule fois. Un code rejoué révoque les
// jetons de rafraîchissement émis pour cet utilisateur et ce client
// (RFC 6749 §4.1.2).
func (s *Store) ConsumeCode(ctx context.Context, code string) (Code, error) {
	var c Code
	var used *time.Time
	var expired bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT user_id::text, client_id, redirect_uri, code_challenge, scope,
			coalesce(nonce, ''), used_at, expires_at < now()
			FROM lattis.oauth_codes WHERE code_hash = $1 FOR UPDATE`, Hash(code)).
			Scan(&c.UserID, &c.ClientID, &c.RedirectURI, &c.CodeChallenge, &c.Scope, &c.Nonce, &used, &expired)
		if err != nil {
			return err
		}
		if used != nil {
			_, err = tx.Exec(ctx, `UPDATE lattis.refresh_tokens SET revoked_at = now()
				WHERE user_id = $1::uuid AND client_id = $2 AND created_at >= $3 AND revoked_at IS NULL`,
				c.UserID, c.ClientID, *used)
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE lattis.oauth_codes SET used_at = now() WHERE code_hash = $1`, Hash(code))
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (used != nil || expired)) {
		return Code{}, ErrNotFound
	}
	return c, err
}

// Refresh est un jeton de rafraîchissement enregistré.
type Refresh struct {
	ID       string
	Family   string
	UserID   string
	ClientID string
	Scope    string
}

// SaveRefresh enregistre un jeton de rafraîchissement ; family vide = nouvelle famille.
func (s *Store) SaveRefresh(ctx context.Context, token string, r Refresh, ttl time.Duration) error {
	_, err := s.DB.Exec(ctx, `INSERT INTO lattis.refresh_tokens (token_hash, family, user_id, client_id, scope, expires_at)
		VALUES ($1, coalesce(nullif($2, '')::uuid, gen_random_uuid()), $3::uuid, $4, $5, now() + make_interval(secs => $6))`,
		Hash(token), r.Family, r.UserID, r.ClientID, r.Scope, ttl.Seconds())
	return err
}

// ErrReused : un jeton de rafraîchissement déjà utilisé a été présenté ;
// toute sa famille est révoquée (détection de réutilisation, §18).
var ErrReused = errors.New("jeton de rafraîchissement réutilisé")

// UseRefresh marque un jeton comme utilisé et renvoie ses données.
func (s *Store) UseRefresh(ctx context.Context, token string) (Refresh, error) {
	var r Refresh
	var used, revoked *time.Time
	var expired bool
	err := pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT id::text, family::text, user_id::text, client_id, scope, used_at, revoked_at, expires_at < now()
			FROM lattis.refresh_tokens WHERE token_hash = $1 FOR UPDATE`, Hash(token)).
			Scan(&r.ID, &r.Family, &r.UserID, &r.ClientID, &r.Scope, &used, &revoked, &expired)
		if err != nil {
			return err
		}
		if used != nil && revoked == nil {
			_, err = tx.Exec(ctx, `UPDATE lattis.refresh_tokens SET revoked_at = now()
				WHERE family = $1::uuid AND revoked_at IS NULL`, r.Family)
			return err
		}
		if used == nil && revoked == nil && !expired {
			_, err = tx.Exec(ctx, `UPDATE lattis.refresh_tokens SET used_at = now() WHERE id = $1::uuid`, r.ID)
		}
		return err
	})
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Refresh{}, ErrNotFound
	case err != nil:
		return Refresh{}, err
	case used != nil && revoked == nil:
		return Refresh{}, ErrReused
	case used != nil || revoked != nil || expired:
		return Refresh{}, ErrNotFound
	}
	return r, nil
}

// RevokeRefresh révoque la famille d'un jeton (déconnexion).
func (s *Store) RevokeRefresh(ctx context.Context, token string) error {
	_, err := s.DB.Exec(ctx, `UPDATE lattis.refresh_tokens SET revoked_at = now()
		WHERE family = (SELECT family FROM lattis.refresh_tokens WHERE token_hash = $1) AND revoked_at IS NULL`, Hash(token))
	return err
}

// Purge supprime les codes et jetons expirés, et les anciennes versions des
// classeurs figés une fois leur délai de grâce passé.
func (s *Store) Purge(ctx context.Context) error {
	_, err := s.DB.Exec(ctx, `
		DELETE FROM lattis.oauth_codes WHERE expires_at < now() - interval '1 day';
		DELETE FROM lattis.refresh_tokens WHERE expires_at < now() - interval '1 day'`)
	if err != nil {
		return err
	}
	_, err = s.DB.Exec(ctx, `SELECT lattis.drop_expired_versions()`)
	return err
}
