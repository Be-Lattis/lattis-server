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
2. Si la base a été migrée par un `lattisd` plus récent, il ne démarre que si aucune des migrations en plus n'est marquée incompatible (voir « Sans interruption ») ; sinon il **refuse de démarrer** : installez une version au moins aussi récente.
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

**Kubernetes** : `helm upgrade lattis deploy/helm/lattis -n lattis -f lattis-values.yaml`. Sans interruption, voir ci-dessous.

Docker Compose fait tourner un seul `lattisd` : la mise à jour le remplace, d'où une coupure de quelques secondes (les postes de bureau gardent leurs connexions à PostgreSQL ; seuls la connexion au compte, l'administration, le relais des changements et l'application web s'interrompent).

Ensuite, la page **État** de l'administration montre la version du schéma, le nombre de connexions PostgreSQL permises et les écarts éventuels.

## Sans interruption (Kubernetes)

Avec le chart par défaut, `helm upgrade` ne coupe pas le service :

- **Deux `lattisd` au moins** (`lattisd.replicaCount: 2`), remplacés un à la fois : un nouveau pod démarre, migre, devient prêt, puis seulement un ancien s'arrête (`maxSurge: 1`, `maxUnavailable: 0`). Un PodDisruptionBudget garde un `lattisd` pendant le vidage d'un nœud.
- **Les anciens pods restent dans le service pendant la migration.** `/readyz` accepte une base en avance tant qu'aucune des migrations en plus n'est marquée incompatible : l'ancienne version continue donc de répondre pendant tout le déploiement.
- **Arrêt en douceur** : un pod qui s'arrête passe `/readyz` en 503, continue de servir `LATTIS_SHUTDOWN_DELAY` (5 s dans le chart) le temps que l'Ingress le retire, ferme les flux `/api/events` avec l'événement `restart` (le poste se réabonne auprès d'un autre `lattisd`), puis finit les requêtes en cours (`LATTIS_SHUTDOWN_GRACE`, 15 s). `terminationGracePeriodSeconds` (45 s) couvre le tout.
- **Retour en arrière sans restauration** : comme l'ancienne version sert une base migrée compatible, `helm rollback` suffit tant qu'aucune migration incompatible n'est passée.

Ce qui s'interrompt encore :

- **Application web** : les moteurs appartiennent au `lattisd` qui les a lancés. Quand ce pod est remplacé, les personnes qu'il servait doivent recharger la page (leur cache est gardé). Avec plusieurs `lattisd`, une personne doit retrouver le pod qui porte son moteur : le chart pose une affinité par cookie pour ingress-nginx (`lattisd.ingress.stickyWeb`) ; avec un autre contrôleur, donnez l'équivalent dans `lattisd.ingress.annotations`.
- **Une migration incompatible** : les anciens pods sortent du service dès qu'elle est appliquée, jusqu'à ce que les nouveaux soient prêts (courte coupure). Les notes de version le signalent.
- **PostgreSQL** : changer l'image du parent le redémarre, pour tout le monde.

### Règle pour écrire une migration

Une migration doit rester **compatible avec la version précédente de `lattisd`**, qui tourne encore pendant le déploiement : ajouter des tables, colonnes (avec valeur par défaut ou NULL) et fonctions, remplacer une fonction en gardant sa signature et son sens. Retirer ou renommer se fait en deux temps : la version N cesse de s'en servir, la version N+1 le retire.

Une migration qui ne peut pas respecter cette règle porte la ligne :

```sql
-- lattis:incompatible
```

Elle est notée comme telle dans `lattis.schema_version` : les `lattisd` plus anciens refusent alors la base (`/readyz` en 503, refus au démarrage), et le retour en arrière demande la restauration d'une sauvegarde.

## Revenir en arrière

Les migrations ne se défont pas. Si aucune migration incompatible n'est passée depuis la version d'avant, il suffit de réinstaller l'ancienne image (`helm rollback`, ou l'ancienne image avec Compose) : elle sert la base migrée. Sinon, un `lattisd` plus ancien refuse de démarrer sur la base (c'est voulu, elle a changé sous lui), et il faut :

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
