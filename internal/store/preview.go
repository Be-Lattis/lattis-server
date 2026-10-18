package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PreviewLimit borne le nombre de lignes renvoyées par « Voir en tant que… ».
const PreviewLimit = 1000

// Preview lit un tableau d'un classeur comme le verrait userID (§8.5).
// actor est le rôle PostgreSQL du concepteur ou propriétaire qui demande.
// La vérification et la journalisation sont faites par
// lattis.authorize_preview ; la lecture a lieu dans une transaction en
// lecture seule sous le rôle de l'utilisateur, si bien que les règles RLS
// et les restrictions s'appliquent exactement comme pour lui.
func (s *Store) Preview(ctx context.Context, actor, workbookID, userID, tableID string) ([]json.RawMessage, error) {
	var role, schema, table string
	err := s.AsActor(ctx, actor, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT lattis.authorize_preview($1, $2::uuid, $3::uuid)`,
			actor, workbookID, userID).Scan(&role); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT schema_name FROM lattis.workbooks WHERE id = $1::uuid`, workbookID).Scan(&schema); err != nil {
			return err
		}
		return tx.QueryRow(ctx, fmt.Sprintf(`SELECT name FROM %s.lattis_meta_tables WHERE table_id = $1::uuid`,
			pgx.Identifier{schema}.Sanitize()), tableID).Scan(&table)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	err = pgx.BeginTxFunc(ctx, s.DB, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+pgx.Identifier{role}.Sanitize()); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, fmt.Sprintf(`SELECT to_jsonb(t) FROM %s t ORDER BY lattis_pos, lattis_id LIMIT %d`,
			pgx.Identifier{schema, table}.Sanitize(), PreviewLimit))
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[json.RawMessage])
		return err
	})
	return out, err
}
