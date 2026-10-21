package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/store"
	"github.com/Be-Lattis/lattis-server/internal/testpg"
)

// Logo du serveur : icône Lattis par défaut, adresse réglée, image envoyée
// (prioritaire, versionnée), retrait.
func TestServerLogo(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	cfg, err := db.Store.Config(ctx)
	if err != nil || cfg.Logo("https://l") != "https://l/logo" {
		t.Fatalf("sans logo : %q %v", cfg.Logo("https://l"), err)
	}
	if _, _, err := db.Store.ServerLogo(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("logo absent : %v", err)
	}
	exec := func(q string, args ...any) error {
		return db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
	}
	if err := exec(`SELECT lattis.set_config('logo_url', '"https://cdn.example/logo.png"')`); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := db.Store.Config(ctx); cfg.Logo("https://l") != "https://cdn.example/logo.png" {
		t.Fatalf("logo par adresse : %q", cfg.Logo("https://l"))
	}
	if err := exec(`SELECT lattis.set_server_logo('image/png', '\x89504e47'::bytea)`); err != nil {
		t.Fatal(err)
	}
	cfg, _ = db.Store.Config(ctx)
	if !strings.HasPrefix(cfg.Logo("https://l"), "https://l/logo?v=") || cfg.LogoVersion == 0 {
		t.Fatalf("logo envoyé : %q", cfg.Logo("https://l"))
	}
	ct, data, err := db.Store.ServerLogo(ctx)
	if err != nil || ct != "image/png" || len(data) != 4 {
		t.Fatalf("lecture : %q %d %v", ct, len(data), err)
	}
	if err := exec(`SELECT lattis.set_server_logo('image/gif', '\x47'::bytea)`); err == nil {
		t.Fatal("type d'image refusé accepté")
	}
	if err := exec(`SELECT lattis.clear_server_logo()`); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := db.Store.Config(ctx); cfg.LogoVersion != 0 {
		t.Fatalf("après retrait : %+v", cfg)
	}
}
