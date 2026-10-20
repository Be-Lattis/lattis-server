package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Be-Lattis/lattis-server/internal/migrate"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Montée de version : état, essai à blanc sans effet, application, refus
// d'une base plus récente que le binaire, migration modifiée signalée.
func TestMigrations(t *testing.T) {
	ctx := context.Background()
	latest := migrate.Latest()
	db := testpg.NewAt(t, latest-2)
	st, err := migrate.Check(ctx, db.Store.DB)
	if err != nil || st.Current != latest-2 || len(st.Pending) != 2 || st.Newer() {
		t.Fatalf("état avant : %+v %v", st, err)
	}
	if n, err := migrate.DryRun(ctx, db.Store.DB); err != nil || n != 2 {
		t.Fatalf("essai à blanc : %d %v", n, err)
	}
	if st, _ := migrate.Check(ctx, db.Store.DB); st.Current != latest-2 {
		t.Fatalf("l'essai à blanc a changé la base : %d", st.Current)
	}
	if n, err := migrate.Up(ctx, db.Store.DB); err != nil || n != 2 {
		t.Fatalf("montée : %d %v", n, err)
	}
	if st, _ := migrate.Check(ctx, db.Store.DB); st.Current != latest || len(st.Pending) != 0 || len(st.Modified) != 0 {
		t.Fatalf("état après : %+v", st)
	}
	// Migration passée dont le fichier aurait changé : signalée, pas rejouée.
	if _, err := db.Super.Exec(ctx, `UPDATE lattis.schema_version SET sha256 = 'autre' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	if st, _ := migrate.Check(ctx, db.Store.DB); len(st.Modified) != 1 {
		t.Fatalf("migration modifiée non signalée : %+v", st)
	}
	// Base migrée par un lattisd plus récent : refus clair.
	if _, err := db.Super.Exec(ctx, `INSERT INTO lattis.schema_version (version, name) VALUES ($1, 'future.sql')`, latest+1); err != nil {
		t.Fatal(err)
	}
	if st, _ := migrate.Check(ctx, db.Store.DB); !st.Newer() || !st.Compatible || !st.Serves() {
		t.Fatalf("base plus récente, compatible : %+v", st)
	}
	// Migration compatible en plus : ce lattisd reste prêt (déploiement
	// progressif, retour à la version précédente).
	if err := migrate.Ready(ctx, db.Store.DB); err != nil {
		t.Fatalf("prêt sur une base en avance compatible : %v", err)
	}
	if _, err := migrate.Up(ctx, db.Store.DB); !errors.Is(err, migrate.ErrNewer) {
		t.Fatalf("montée sur une base plus récente : %v", err)
	}
	// Migration incompatible au-delà : plus prêt, refus au démarrage.
	if _, err := db.Super.Exec(ctx, `INSERT INTO lattis.schema_version (version, name, incompatible) VALUES ($1, 'future2.sql', true)`, latest+2); err != nil {
		t.Fatal(err)
	}
	if st, _ := migrate.Check(ctx, db.Store.DB); st.Compatible || st.Serves() {
		t.Fatalf("migration incompatible non vue : %+v", st)
	}
	if err := migrate.Ready(ctx, db.Store.DB); !errors.Is(err, migrate.ErrNewer) {
		t.Fatalf("prêt malgré une migration incompatible : %v", err)
	}
}
