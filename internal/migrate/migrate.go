// Package migrate applique les migrations du schéma lattis.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

// ErrNewer : la base a été migrée par un lattisd plus récent que celui-ci.
var ErrNewer = errors.New("schéma plus récent que ce lattisd")

// Status : état des migrations d'une base.
type Status struct {
	Current, Latest int
	Pending         []string // migrations à appliquer
	// Modified : migrations appliquées dont le fichier embarqué a changé
	// depuis (à examiner : la base ne les rejouera pas).
	Modified []string
}

// Newer : la base est plus récente que les migrations embarquées.
func (s Status) Newer() bool { return s.Current > s.Latest }

func checksum(m Migration) string {
	h := sha256.Sum256([]byte(m.SQL))
	return hex.EncodeToString(h[:])
}

// ensure crée ou complète la table des versions (empreinte de chaque fichier).
func ensure(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}) error {
	_, err := q.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS lattis;
		CREATE TABLE IF NOT EXISTS lattis.schema_version (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		);
		ALTER TABLE lattis.schema_version ADD COLUMN IF NOT EXISTS sha256 text;
		ALTER TABLE lattis.schema_version ADD COLUMN IF NOT EXISTS lattisd text`)
	if err != nil {
		return fmt.Errorf("table schema_version : %w", err)
	}
	return nil
}

// Check lit l'état sans rien modifier.
func Check(ctx context.Context, db *pgxpool.Pool) (Status, error) {
	all, err := All()
	if err != nil {
		return Status{}, err
	}
	st := Status{Latest: len(all)}
	cur, err := Current(ctx, db)
	if err != nil {
		return st, err
	}
	st.Current = cur
	sums := map[int]string{}
	if cur > 0 {
		var hasSum bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
			WHERE table_schema = 'lattis' AND table_name = 'schema_version' AND column_name = 'sha256')`).Scan(&hasSum); err != nil {
			return st, err
		}
		if hasSum {
			rows, err := db.Query(ctx, `SELECT version, coalesce(sha256, '') FROM lattis.schema_version`)
			if err != nil {
				return st, err
			}
			for rows.Next() {
				var v int
				var s string
				if err := rows.Scan(&v, &s); err != nil {
					rows.Close()
					return st, err
				}
				sums[v] = s
			}
			if err := rows.Err(); err != nil {
				return st, err
			}
		}
	}
	for _, m := range all {
		switch {
		case m.Version > cur:
			st.Pending = append(st.Pending, m.Name)
		case sums[m.Version] != "" && sums[m.Version] != checksum(m):
			st.Modified = append(st.Modified, m.Name)
		}
	}
	return st, nil
}

// Version : version de lattisd notée avec chaque migration appliquée.
var Version = "dev"

// UpTo applique les migrations manquantes jusqu'à la version max comprise
// (tests de mise à jour depuis une version précédente), chacune dans sa
// transaction, sous un verrou consultatif (plusieurs lattisd au démarrage).
// Une base plus récente que ce lattisd est refusée (ErrNewer).
func UpTo(ctx context.Context, db *pgxpool.Pool, max int) (int, error) {
	return run(ctx, db, max, false)
}

// DryRun applique les migrations manquantes dans une seule transaction,
// puis l'annule : rien ne change, mais une migration qui échouerait sur
// cette base échoue ici. Renvoie les migrations essayées.
func DryRun(ctx context.Context, db *pgxpool.Pool) (int, error) {
	return run(ctx, db, 1<<30, true)
}

var errRollback = errors.New("essai à blanc")

func run(ctx context.Context, db *pgxpool.Pool, max int, dry bool) (int, error) {
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

	if !dry {
		if err := ensure(ctx, conn); err != nil {
			return 0, err
		}
	}
	var current int
	if err := conn.QueryRow(ctx, `SELECT CASE WHEN to_regclass('lattis.schema_version') IS NULL THEN 0
		ELSE (SELECT coalesce(max(version), 0) FROM lattis.schema_version) END`).Scan(&current); err != nil {
		return 0, err
	}
	if current > len(all) {
		return 0, fmt.Errorf("%w : base en version %d, ce lattisd s'arrête à %d ; installez un lattisd au moins aussi récent que celui qui a migré la base", ErrNewer, current, len(all))
	}
	if !dry {
		// Empreintes des migrations passées avant qu'elles soient notées.
		for _, m := range all {
			if m.Version <= current {
				if _, err := conn.Exec(ctx, `UPDATE lattis.schema_version SET sha256 = $2 WHERE version = $1 AND sha256 IS NULL`, m.Version, checksum(m)); err != nil {
					return 0, err
				}
			}
		}
	}
	apply := func(tx pgx.Tx, m Migration) error {
		if _, err := tx.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("migration %s : %w", m.Name, err)
		}
		if dry {
			return nil
		}
		_, err := tx.Exec(ctx, `INSERT INTO lattis.schema_version (version, name, sha256, lattisd) VALUES ($1, $2, $3, $4)`,
			m.Version, m.Name, checksum(m), Version)
		return err
	}
	applied := 0
	if dry {
		err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			for _, m := range all {
				if m.Version <= current || m.Version > max {
					continue
				}
				if err := apply(tx, m); err != nil {
					return err
				}
				applied++
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			err = nil
		}
		return applied, err
	}
	for _, m := range all {
		if m.Version <= current || m.Version > max {
			continue
		}
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error { return apply(tx, m) }); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}
