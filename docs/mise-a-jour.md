# Mettre à jour le Serveur Lattis

Une mise à jour se fait sans commande à la main : la nouvelle version applique d'elle-même ses migrations du schéma `lattis` et ses réglages PostgreSQL. Ce guide dit ce qui se passe, ce qu'il faut faire avant, et comment revenir en arrière.

## Ce qui se passe au démarrage

**PostgreSQL** (image `be-lattis/server`) : à chaque démarrage, le parent et les réplicas réécrivent `$PGDATA/lattis.conf`, inclus en fin de `postgresql.conf`. Ce fichier contient :
- `max_connections` (`LATTIS_PG_MAX_CONNECTIONS`, 400) ;
- `hot_standby_feedback` ;
- `max_standby_streaming_delay` (`LATTIS_PG_STANDBY_DELAY`, 30s).

Une nouvelle image apporte donc ses réglages sans `ALTER SYSTEM`. Ne modifiez pas `lattis.conf` : mettez vos propres réglages dans `postgresql.conf` avant la ligne `include_if_exists`, ou dans `postgresql.auto.conf`.

**`lattisd`** :

1. Il lit la version du schéma (`lattis.schema_version`).
2. Si la base a été migrée par un `lattisd` plus récent, il **refuse de démarrer** : installez une version au moins aussi récente.
3. S'il manque des migrations :
   - avec `--migrate` (Compose et chart Helm par défaut), il les applique, chacune dans sa transaction ;
   - sans `--migrate`, il refuse de démarrer et dit quelle commande lancer.
4. Plusieurs `lattisd` qui démarrent ensemble se rangent derrière un verrou : un seul migre, les autres attendent puis trouvent la base à jour.
5. Chaque migration appliquée est notée avec son empreinte (SHA-256) et la version de `lattisd`. Une migration déjà passée dont le fichier aurait changé n'est pas rejouée : elle est signalée dans le journal, dans la page **État** de l'administration et par `lattisd migrate --status`.

Une migration qui échoue est annulée entièrement : la base reste à la migration précédente, `lattisd` s'arrête avec l'erreur, et rien n'est à moitié appliqué.

## Avant de mettre à jour

1. **Lire les notes de version** de la release (GitHub) : elles signalent ce qui demande une attention particulière.
2. **Sauvegarder** la base :

   ```sh
   docker compose exec postgres gosu postgres pg_dump -Fc lattis > lattis-avant-maj.dump
   docker compose exec postgres gosu postgres pg_dumpall --roles-only > roles-avant-maj.sql
   ```

   Les rôles sont communs à l'instance PostgreSQL (un par utilisateur et par classeur) : `pg_dump` seul ne les contient pas.
3. **Essayer à blanc** (facultatif, conseillé sur une grosse base) avec la nouvelle image :

   ```sh
   docker compose run --rm lattisd migrate --status
   docker compose run --rm lattisd migrate --dry-run
   ```

   `--dry-run` applique les migrations en attente dans une seule transaction puis l'annule : une migration qui échouerait sur votre base échoue ici, sans rien changer.

## Mettre à jour

**Docker Compose** :

```sh
git pull                      # ou la nouvelle release
cd deploy/compose
docker compose up -d --build
docker compose logs lattisd | grep -i migration
```

**Kubernetes** : `helm upgrade lattis deploy/helm/lattis -n lattis -f lattis-values.yaml`. Avec plusieurs `lattisd`, le nouveau pod migre au démarrage ; les anciens pods continuent de servir pendant ce temps (les migrations ajoutent sans retirer ce dont l'ancienne version a besoin, sauf mention contraire dans les notes de version).

Ensuite, la page **État** de l'administration montre la version du schéma, le nombre de connexions PostgreSQL permises et les écarts éventuels.

## Revenir en arrière

Les migrations ne se défont pas : un `lattisd` plus ancien refuse de démarrer sur une base migrée par un plus récent (c'est voulu, il ne connaît pas les nouvelles tables). Pour revenir en arrière :

1. arrêter `lattisd` ;
2. restaurer la sauvegarde :

   ```sh
   docker compose exec -T postgres gosu postgres psql < roles-avant-maj.sql
   docker compose exec -T postgres gosu postgres pg_restore --clean --if-exists -d lattis < lattis-avant-maj.dump
   ```

3. redémarrer avec l'ancienne image.

## Commandes

| Commande | Effet |
|---|---|
| `lattisd --migrate` | Démarre en appliquant les migrations en attente. |
| `lattisd` | Démarre seulement si la base est à jour. |
| `lattisd migrate` | Applique les migrations en attente puis s'arrête. |
| `lattisd migrate --status` | Montre la version de la base et ce qui manque, sans rien changer. |
| `lattisd migrate --dry-run` | Essaie les migrations en attente puis annule tout. |
| `lattisd version` | Version de `lattisd`. |
