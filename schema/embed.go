// Package schema embarque les migrations SQL du schéma lattis.
package schema

import "embed"

// Migrations contient les fichiers NNNN_nom.sql, appliqués dans l'ordre.
//
//go:embed [0-9][0-9][0-9][0-9]_*.sql
var Migrations embed.FS

// Bootstrap est le script d'amorçage exécuté par un superutilisateur.
//
//go:embed bootstrap.sql
var Bootstrap string
