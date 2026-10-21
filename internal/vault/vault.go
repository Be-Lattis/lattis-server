// Package vault garde en base les secrets saisis dans l'administration
// (secret d'un fournisseur d'identité, mot de passe d'une source de
// pipeline) : chiffrés (AES-256-GCM) par une clé tirée de la clé de
// signature de lattisd, lisibles du seul compte de service. Un secret se
// désigne par une référence db-<32 hex>, à la place d'un nom de fichier du
// dossier des secrets.
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

const label = "lattis-vault-v1"

var refRe = regexp.MustCompile(`^db-[0-9a-f]{32}$`)

// IsRef : la référence désigne un secret gardé en base.
func IsRef(ref string) bool { return refRe.MatchString(ref) }

// Vault scelle et lit les secrets.
type Vault struct {
	Pool *pgxpool.Pool
	Keys *tokens.KeySet
}

// Sealed : secret prêt à enregistrer (lattis.store_secret).
type Sealed struct {
	Ref, KeyID string
	Data       []byte
}

// Seal chiffre value sous une référence neuve.
func (v *Vault) Seal(value string) (Sealed, error) {
	kid, key := v.Keys.Derive(label)
	data, err := seal(key, []byte(value))
	if err != nil {
		return Sealed{}, err
	}
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return Sealed{Ref: "db-" + hex.EncodeToString(b), KeyID: kid, Data: data}, nil
}

// Store enregistre value sous l'identité de tx (propriétaire du serveur) en
// remplaçant old s'il désigne un secret en base ; renvoie la référence.
func (v *Vault) Store(ctx context.Context, tx pgx.Tx, value, old string) (string, error) {
	s, err := v.Seal(value)
	if err != nil {
		return "", err
	}
	var prev any
	if IsRef(old) {
		prev = old
	}
	_, err = tx.Exec(ctx, `SELECT lattis.store_secret($1, $2, $3, $4)`, s.Ref, s.KeyID, s.Data, prev)
	return s.Ref, err
}

// Get lit un secret (compte de service).
func (v *Vault) Get(ctx context.Context, ref string) (string, error) {
	var kid string
	var data []byte
	err := v.Pool.QueryRow(ctx, `SELECT key_id, sealed FROM lattis.stored_secrets WHERE name = $1`, ref).Scan(&kid, &data)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("secret %s introuvable : saisissez-le de nouveau dans l'administration", ref)
	}
	if err != nil {
		return "", err
	}
	key, ok := v.Keys.DeriveFor(kid, label)
	if !ok {
		return "", fmt.Errorf("secret %s chiffré par une clé de signature retirée : saisissez-le de nouveau dans l'administration", ref)
	}
	plain, err := open(key, data)
	if err != nil {
		return "", fmt.Errorf("secret %s illisible : %w", ref, err)
	}
	return string(plain), nil
}

// Reseal rechiffre par la clé de signature courante les secrets scellés par
// une clé plus ancienne encore présente (rotation de la clé de signature).
func (v *Vault) Reseal(ctx context.Context) error {
	kid, key := v.Keys.Derive(label)
	rows, err := v.Pool.Query(ctx, `SELECT name, key_id, sealed FROM lattis.stored_secrets WHERE key_id <> $1`, kid)
	if err != nil {
		return err
	}
	type item struct {
		name, kid string
		data      []byte
	}
	var todo []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.name, &it.kid, &it.data); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, it)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, it := range todo {
		old, ok := v.Keys.DeriveFor(it.kid, label)
		if !ok {
			slog.Warn("secret chiffré par une clé de signature retirée : à saisir de nouveau", "ref", it.name)
			continue
		}
		plain, err := open(old, it.data)
		if err != nil {
			slog.Warn("secret illisible", "ref", it.name, "err", err)
			continue
		}
		data, err := seal(key, plain)
		if err != nil {
			return err
		}
		if _, err := v.Pool.Exec(ctx, `UPDATE lattis.stored_secrets SET key_id = $2, sealed = $3 WHERE name = $1`, it.name, kid, data); err != nil {
			return err
		}
	}
	return nil
}

func seal(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte(label)), nil
}

func open(key, data []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("donnée trop courte")
	}
	return gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(label))
}
