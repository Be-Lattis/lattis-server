package store_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Be-Lattis/lattis-server/internal/testpg"
	"github.com/Be-Lattis/lattis-server/internal/tokens"
	"github.com/Be-Lattis/lattis-server/internal/vault"
)

// Secret saisi en valeur : chiffré en base, relu par le compte de service,
// remplacé sans rester ; illisible sans la clé de signature ; rechiffré
// après une rotation de clé.
func TestVault(t *testing.T) {
	ctx := context.Background()
	db := testpg.New(t)
	pem, _ := tokens.GenerateKeyPEM()
	keys, err := tokens.ParseKeys(pem)
	if err != nil {
		t.Fatal(err)
	}
	v := &vault.Vault{Pool: db.Store.DB, Keys: keys}
	var ref, ref2 string
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		var err error
		ref, err = v.Store(ctx, tx, "s3cr3t-du-client", "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !vault.IsRef(ref) {
		t.Fatalf("référence : %q", ref)
	}
	var raw []byte
	if err := db.Super.QueryRow(ctx, `SELECT sealed FROM lattis.stored_secrets WHERE name = $1`, ref).Scan(&raw); err != nil || strings.Contains(string(raw), "s3cr3t") {
		t.Fatalf("secret en clair en base : %v", err)
	}
	if got, err := v.Get(ctx, ref); err != nil || got != "s3cr3t-du-client" {
		t.Fatalf("lecture : %q %v", got, err)
	}
	// Remplacement : l'ancien disparaît.
	if err := db.Store.AsActor(ctx, "test", func(tx pgx.Tx) error {
		var err error
		ref2, err = v.Store(ctx, tx, "nouveau", ref)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get(ctx, ref); err == nil {
		t.Fatal("ancien secret encore là")
	}
	// Rotation : nouvelle clé en tête, ancienne gardée ; rechiffré.
	pem2, _ := tokens.GenerateKeyPEM()
	keys2, _ := tokens.ParseKeys(append(pem2, pem...))
	v2 := &vault.Vault{Pool: db.Store.DB, Keys: keys2}
	if err := v2.Reseal(ctx); err != nil {
		t.Fatal(err)
	}
	keys3, _ := tokens.ParseKeys(pem2) // ancienne clé retirée
	if got, err := (&vault.Vault{Pool: db.Store.DB, Keys: keys3}).Get(ctx, ref2); err != nil || got != "nouveau" {
		t.Fatalf("après rotation : %q %v", got, err)
	}
	// Sans la bonne clé : erreur claire, pas de contenu.
	pem4, _ := tokens.GenerateKeyPEM()
	keys4, _ := tokens.ParseKeys(pem4)
	if _, err := (&vault.Vault{Pool: db.Store.DB, Keys: keys4}).Get(ctx, ref2); err == nil || !strings.Contains(err.Error(), "saisissez-le de nouveau") {
		t.Fatalf("clé inconnue : %v", err)
	}
}
