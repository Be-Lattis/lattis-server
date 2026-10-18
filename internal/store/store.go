// Package store est l'accès de lattisd au schéma lattis, sous le rôle
// technique lattis_service.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store enveloppe le pool de connexions de service.
type Store struct {
	DB *pgxpool.Pool
}

// Open ouvre le pool vers le parent.
func Open(ctx context.Context, url string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "lattisd"
	db, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{DB: db}, nil
}

// Close ferme le pool.
func (s *Store) Close() { s.DB.Close() }

// Hash est le hachage SHA-256 conservé pour les codes et jetons opaques.
func Hash(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// ErrNotFound : rien ne correspond.
var ErrNotFound = errors.New("introuvable")

// AsActor exécute fn dans une transaction où les fonctions lattis.*
// journalisent l'action au nom de actor.
func (s *Store) AsActor(ctx context.Context, actor string, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.DB, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lattis.actor', $1, true)`, actor); err != nil {
			return err
		}
		return fn(tx)
	})
}

// --- Réglages -----------------------------------------------------------------

// ServerConfig est une vue typée de lattis.server_config.
type ServerConfig struct {
	DisplayName         string
	LogoURL             string
	AccessMode          string
	DefaultRole         string
	WebScripting        bool
	AccessTokenTTL      time.Duration
	RefreshTokenTTL     time.Duration
	PasswordMaxFailures int
	PasswordLock        time.Duration
}

// Config lit les réglages du serveur.
func (s *Store) Config(ctx context.Context) (ServerConfig, error) {
	rows, err := s.DB.Query(ctx, `SELECT key, value FROM lattis.server_config`)
	if err != nil {
		return ServerConfig{}, err
	}
	raw := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err != nil {
			return ServerConfig{}, err
		}
		raw[k] = v
	}
	if err := rows.Err(); err != nil {
		return ServerConfig{}, err
	}
	c := ServerConfig{
		DisplayName: "Serveur Lattis", AccessMode: "invitation", DefaultRole: "none",
		AccessTokenTTL: 15 * time.Minute, RefreshTokenTTL: 30 * 24 * time.Hour,
		PasswordMaxFailures: 5, PasswordLock: 15 * time.Minute,
	}
	str := func(k string, dst *string) {
		var v *string
		if json.Unmarshal(raw[k], &v) == nil && v != nil {
			*dst = *v
		}
	}
	secs := func(k string, dst *time.Duration) {
		var v float64
		if json.Unmarshal(raw[k], &v) == nil && v > 0 {
			*dst = time.Duration(v * float64(time.Second))
		}
	}
	str("display_name", &c.DisplayName)
	str("logo_url", &c.LogoURL)
	str("access_mode", &c.AccessMode)
	str("default_role", &c.DefaultRole)
	_ = json.Unmarshal(raw["web_scripting"], &c.WebScripting)
	secs("access_token_ttl", &c.AccessTokenTTL)
	secs("refresh_token_ttl", &c.RefreshTokenTTL)
	secs("password_lock_seconds", &c.PasswordLock)
	var n int
	if json.Unmarshal(raw["password_max_failures"], &n) == nil && n > 0 {
		c.PasswordMaxFailures = n
	}
	return c, nil
}

// --- Fournisseurs et tenants ----------------------------------------------------

// Provider est un fournisseur d'identité (lattis.identity_providers).
type Provider struct {
	ID         string
	Kind       string
	Label      string
	Issuer     string
	ClientID   string
	SecretRef  string
	Scopes     []string
	ClaimName  string
	ClaimValue string
	Enabled    bool
	Position   int
}

const providerCols = `id::text, kind, label, coalesce(issuer, ''), coalesce(client_id, ''),
	coalesce(client_secret_ref, ''), scopes, coalesce(claim_name, ''), coalesce(claim_value, ''), enabled, position`

func scanProvider(row pgx.Row) (Provider, error) {
	var p Provider
	err := row.Scan(&p.ID, &p.Kind, &p.Label, &p.Issuer, &p.ClientID, &p.SecretRef, &p.Scopes,
		&p.ClaimName, &p.ClaimValue, &p.Enabled, &p.Position)
	return p, err
}

// Providers liste les fournisseurs actifs, dans l'ordre d'affichage.
func (s *Store) Providers(ctx context.Context) ([]Provider, error) {
	rows, err := s.DB.Query(ctx, `SELECT `+providerCols+` FROM lattis.identity_providers
		WHERE enabled ORDER BY position, label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Provider lit un fournisseur actif.
func (s *Store) Provider(ctx context.Context, id string) (Provider, error) {
	p, err := scanProvider(s.DB.QueryRow(ctx, `SELECT `+providerCols+` FROM lattis.identity_providers
		WHERE id::text = $1 AND enabled`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

// Tenant est un sous-ensemble autorisé d'un fournisseur.
type Tenant struct {
	ID          string
	Kind        string
	Value       string
	DefaultRole string
}

// Tenants liste les tenants autorisés d'un fournisseur.
func (s *Store) Tenants(ctx context.Context, providerID string) ([]Tenant, error) {
	rows, err := s.DB.Query(ctx, `SELECT id::text, kind, value, coalesce(default_role, '')
		FROM lattis.allowed_tenants WHERE provider_id::text = $1`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tenant
	for rows.Next() {
		var t Tenant
		if err := rows.Scan(&t.ID, &t.Kind, &t.Value, &t.DefaultRole); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// --- Utilisateurs ----------------------------------------------------------------

// User est un utilisateur (lattis.users).
type User struct {
	ID          string
	PGRole      string
	Email       string
	DisplayName string
	Disabled    bool
}

// User lit un utilisateur.
func (s *Store) User(ctx context.Context, id string) (User, error) {
	var u User
	err := s.DB.QueryRow(ctx, `SELECT id::text, pg_role, email, display_name, disabled
		FROM lattis.users WHERE id::text = $1`, id).Scan(&u.ID, &u.PGRole, &u.Email, &u.DisplayName, &u.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

// Identity est l'identité vérifiée auprès d'un fournisseur.
type Identity struct {
	ProviderID    string
	Subject       string
	Email         string
	EmailVerified bool
	DisplayName   string
	// TenantDefaultRole : rôle par défaut du tenant qui a laissé passer
	// l'utilisateur (vide = celui du serveur).
	TenantDefaultRole string
}

// Enrollment est le résultat de lattis.enroll.
type Enrollment struct {
	Status      string `json:"status"` // ok, disabled, not_invited
	UserID      string `json:"user_id"`
	PGRole      string `json:"pg_role"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Mode        string `json:"mode"` // existing, invitation, open
}

// Enroll applique le mode d'accès et crée ou active le compte.
func (s *Store) Enroll(ctx context.Context, id Identity) (Enrollment, error) {
	var raw []byte
	var tdr *string
	if id.TenantDefaultRole != "" {
		tdr = &id.TenantDefaultRole
	}
	var e Enrollment
	err := s.AsActor(ctx, "lattisd", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.enroll($1::uuid, $2, $3, $4, $5, $6)`,
			id.ProviderID, id.Subject, id.Email, id.EmailVerified, id.DisplayName, tdr).Scan(&raw)
	})
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return e, fmt.Errorf("lattis.enroll : %w", err)
	}
	return e, nil
}

// --- Comptes à mot de passe --------------------------------------------------------

// PasswordAccount est un compte du fournisseur « mot de passe ».
type PasswordAccount struct {
	UserID         string
	ProviderID     string
	Hash           string
	FailedAttempts int
	LockedUntil    *time.Time
	Disabled       bool
}

// PasswordAccount cherche le compte à mot de passe d'une adresse.
func (s *Store) PasswordAccount(ctx context.Context, email string) (PasswordAccount, error) {
	var a PasswordAccount
	err := s.DB.QueryRow(ctx, `
		SELECT u.id::text, p.id::text, coalesce(c.password_hash, ''), coalesce(c.failed_attempts, 0), c.locked_until, u.disabled
		FROM lattis.users u
		JOIN lattis.identity_providers p ON p.id = u.provider_id AND p.kind = 'password' AND p.enabled
		LEFT JOIN lattis.local_credentials c ON c.user_id = u.id
		WHERE u.email = lower($1)`, email).
		Scan(&a.UserID, &a.ProviderID, &a.Hash, &a.FailedAttempts, &a.LockedUntil, &a.Disabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// PasswordFailed compte un échec et verrouille le compte au-delà de max.
func (s *Store) PasswordFailed(ctx context.Context, userID string, max int, lock time.Duration) error {
	_, err := s.DB.Exec(ctx, `
		UPDATE lattis.local_credentials
		SET failed_attempts = failed_attempts + 1,
		    locked_until = CASE WHEN failed_attempts + 1 >= $2 THEN now() + make_interval(secs => $3) ELSE locked_until END
		WHERE user_id::text = $1`, userID, max, lock.Seconds())
	return err
}

// PasswordSucceeded remet le compteur d'échecs à zéro.
func (s *Store) PasswordSucceeded(ctx context.Context, userID string) error {
	_, err := s.DB.Exec(ctx, `UPDATE lattis.local_credentials SET failed_attempts = 0, locked_until = NULL
		WHERE user_id::text = $1`, userID)
	return err
}

// SetPasswordWithToken consomme un jeton de choix de mot de passe.
func (s *Store) SetPasswordWithToken(ctx context.Context, token, hash string) (string, error) {
	var id *string
	err := s.AsActor(ctx, "lattisd", func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT lattis.set_password_with_token($1, $2)::text`, token, hash).Scan(&id)
	})
	if err != nil {
		return "", err
	}
	if id == nil {
		return "", ErrNotFound
	}
	return *id, nil
}

// SetupTokenValid dit si un jeton de choix de mot de passe est utilisable,
// et pour quelle adresse.
func (s *Store) SetupTokenValid(ctx context.Context, token string) (string, bool, error) {
	var email string
	err := s.DB.QueryRow(ctx, `SELECT u.email FROM lattis.local_credentials c JOIN lattis.users u ON u.id = c.user_id
		WHERE c.setup_token_hash = $1 AND c.setup_expires_at > now()`, Hash(token)).Scan(&email)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return email, err == nil, err
}

// ServerOwner dit si l'utilisateur est propriétaire du serveur, ou le
// deviendra à sa première connexion (invitation en attente).
func (s *Store) ServerOwner(ctx context.Context, userID string) (bool, error) {
	var owner bool
	err := s.DB.QueryRow(ctx, `
		SELECT pg_has_role(u.pg_role, 'lattis_server_owners', 'MEMBER')
		    OR EXISTS (SELECT 1 FROM lattis.invitations i
		               WHERE i.email = u.email AND 'server_owner' = ANY (i.roles) AND i.accepted_at IS NULL
		                 AND i.revoked_at IS NULL AND i.expires_at > now())
		FROM lattis.users u WHERE u.id::text = $1 AND NOT u.disabled`, userID).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return owner, err
}

// --- Amorçage --------------------------------------------------------------------

// Bootstrap invite le premier propriétaire de serveur s'il n'y en a aucun.
// Renvoie le jeton de choix de mot de passe (vide si rien n'a été fait).
func (s *Store) Bootstrap(ctx context.Context, email string) (string, error) {
	var token string
	err := s.AsActor(ctx, "lattisd (amorçage)", func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(0x1a77150002)`); err != nil {
			return err
		}
		var has bool
		err := tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM lattis.users
			               WHERE NOT disabled AND pg_has_role(pg_role, 'lattis_server_owners', 'MEMBER'))
			    OR EXISTS (SELECT 1 FROM lattis.invitations
			               WHERE 'server_owner' = ANY (roles) AND accepted_at IS NULL
			                 AND revoked_at IS NULL AND expires_at > now())`).Scan(&has)
		if err != nil || has {
			return err
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lattis.users u JOIN lattis.identity_providers p
			ON p.id = u.provider_id AND p.kind = 'password' WHERE u.email = lower($1))`, email).Scan(&exists); err != nil {
			return err
		}
		if exists {
			err = tx.QueryRow(ctx, `SELECT lattis.reset_password(u.id) FROM lattis.users u
				JOIN lattis.identity_providers p ON p.id = u.provider_id AND p.kind = 'password'
				WHERE u.email = lower($1)`, email).Scan(&token)
		} else {
			err = tx.QueryRow(ctx, `SELECT lattis.create_password_account($1, '')`, email).Scan(&token)
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `SELECT lattis.invite($1, NULL, '{server_owner}')`, email)
		return err
	})
	return token, err
}
