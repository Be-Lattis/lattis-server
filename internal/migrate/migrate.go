// Package migrate applique les migrations du schéma lattis.
package migrate

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Be-Lattis/lattis-server/schema"
)

// Migration est un fichier NNNN_nom.sql.
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// lockKey sert de verrou consultatif : deux lattisd qui démarrent ensemble
// n'appliquent pas les migrations en même temps.
const lockKey = 0x1a77150001

// All renvoie les migrations embarquées, triées.
func All() ([]Migration, error) {
	entries, err := fs.ReadDir(schema.Migrations, ".")
	if err != nil {
		return nil, err
	}
	var out []Migration
	for _, e := range entries {
		name := e.Name()
		num, rest, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(rest, ".sql") {
			continue
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s : numéro invalide", name)
		}
		body, err := fs.ReadFile(schema.Migrations, name)
		if err != nil {
			return nil, err
		}
		out = append(out, Migration{Version: v, Name: name, SQL: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i := range out {
		if out[i].Version != i+1 {
			return nil, fmt.Errorf("migration %s : numérotation non continue", out[i].Name)
		}
	}
	return out, nil
}

// Latest est la dernière version embarquée.
func Latest() int {
	all, err := All()
	if err != nil || len(all) == 0 {
		return 0
	}
	return all[len(all)-1].Version
}

// Current lit la version appliquée (0 si le schéma n'existe pas).
func Current(ctx context.Context, db *pgxpool.Pool) (int, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('lattis.schema_version') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var v int
	err := db.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM lattis.schema_version`).Scan(&v)
	return v, err
}

// Up applique les migrations manquantes, chacune dans sa transaction.
// Renvoie le nombre de migrations appliquées.
func Up(ctx context.Context, db *pgxpool.Pool) (int, error) {
	return UpTo(ctx, db, 1<<30)
}

// UpTo applique les migrations manquantes jusqu'à la version max comprise
// (tests de mise à jour depuis une version précédente).
func UpTo(ctx context.Context, db *pgxpool.Pool, max int) (int, error) {
	all, err := All()
	if err != nil {
		return 0, err
	}
	conn, err := db.Acquire(ctx)
	if err != nil {
		return 0, err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, int64(lockKey)); err != nil {
		return 0, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, int64(lockKey))

	if _, err := conn.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS lattis;
		CREATE TABLE IF NOT EXISTS lattis.schema_version (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return 0, fmt.Errorf("table schema_version : %w", err)
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT coalesce(max(version), 0) FROM lattis.schema_version`).Scan(&current); err != nil {
		return 0, err
	}
	applied := 0
	for _, m := range all {
		if m.Version <= current || m.Version > max {
			continue
		}
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, m.SQL); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO lattis.schema_version (version, name) VALUES ($1, $2)`, m.Version, m.Name)
			return err
		})
		if err != nil {
			return applied, fmt.Errorf("migration %s : %w", m.Name, err)
		}
		applied++
	}
	return applied, nil
}
