// Package testpg prépare une base PostgreSQL 18 jetable pour les tests
// d'intégration. Les tests sont ignorés sans LATTIS_TEST_PG_URL (URL d'un
// superutilisateur, par exemple postgres://postgres:…@127.0.0.1:5432/postgres).
package testpg

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
)

// ServicePassword est le mot de passe donné à lattis_service pendant les tests.
const ServicePassword = "lattis-service-test"

// DB est une base de test migrée.
type DB struct {
	Name     string
	Super    *pgxpool.Pool // superutilisateur, sur la base de test
	Store    *store.Store  // lattis_service
	SuperURL string
}

// New crée une base neuve, l'amorce comme le script d'initialisation de
// l'image et applique les migrations.
func New(t *testing.T) *DB {
	t.Helper()
	return NewAt(t, migrate.Latest())
}

// NewAt crée une base de test migrée jusqu'à la version donnée seulement.
func NewAt(t *testing.T, version int) *DB {
	t.Helper()
	admin := os.Getenv("LATTIS_TEST_PG_URL")
	if admin == "" {
		t.Skip("LATTIS_TEST_PG_URL non défini : test d'intégration ignoré")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	name := "lattis_test_" + strings.ToLower(strings.ReplaceAll(tokens.NewID(), "-", "_"))[:12]
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	// Équivalent de schema/bootstrap.sql (qui utilise des méta-commandes psql).
	stmts := []string{
		`DO $$ BEGIN
		   IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'lattis_service') THEN
		     CREATE ROLE lattis_service LOGIN CREATEROLE;
		   END IF;
		 END $$`,
		fmt.Sprintf(`ALTER ROLE lattis_service PASSWORD '%s'`, ServicePassword),
		`GRANT CREATE, CONNECT ON DATABASE ` + pgx.Identifier{name}.Sanitize() + ` TO lattis_service`,
		`GRANT pg_monitor, pg_signal_backend TO lattis_service`,
	}
	// Rôles communs à tout le serveur PostgreSQL : les paquets de tests qui
	// tournent en parallèle s'amorcent l'un après l'autre.
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(7357)`); err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("amorçage : %v", err)
		}
	}
	// Le verrou court jusqu'aux migrations, qui créent aussi des rôles
	// communs (lattis_server_owners, lattis_workbook_creators).
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(7357)`)
	superURL := withDB(t, admin, name, "", "")
	svcURL := withDB(t, admin, name, "lattis_service", ServicePassword)
	super, err := pgxpool.New(ctx, superURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(super.Close)
	st, err := store.Open(ctx, svcURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if _, err := migrate.UpTo(ctx, st.DB, version); err != nil {
		t.Fatal(err)
	}
	return &DB{Name: name, Super: super, Store: st, SuperURL: superURL}
}

func withDB(t *testing.T, raw, db, user, pw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + db
	if user != "" {
		u.User = url.UserPassword(user, pw)
	}
	return u.String()
}

// AsUser exécute fn sur une connexion dont session_user est role (comme si
// l'utilisateur s'était connecté avec son jeton).
func (d *DB) AsUser(t *testing.T, role string, fn func(pgx.Tx) error) error {
	t.Helper()
	ctx := context.Background()
	return pgx.BeginFunc(ctx, d.Super, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL SESSION AUTHORIZATION "+pgx.Identifier{role}.Sanitize()); err != nil {
			return err
		}
		return fn(tx)
	})
}

// Email renvoie une adresse unique (les rôles PostgreSQL sont communs à
// toutes les bases du serveur de test).
func Email(prefix string) string {
	return strings.ToLower(prefix + "." + tokens.NewID()[:8] + "@example.test")
}
