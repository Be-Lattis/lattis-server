# Montée en charge du Serveur Lattis

Ce qui consomme quand le nombre de personnes augmente, où ça bloque, et ce qu'on règle. Les chiffres sont des ordres de grandeur : l'écran **Surveillance** de l'administration montre l'occupation réelle (utilisateurs, moteurs, mémoire sur la dernière heure).

## Application de bureau

Un poste parle directement à PostgreSQL, sous son propre rôle, et à `lattisd` seulement pour se connecter, renouveler ses jetons et suivre les changements.

| Ressource | Par poste | Goulet | Ce qu'on règle |
|---|---|---|---|
| Connexions au PostgreSQL parent | 1 ouverte en permanence (synchronisation), parfois 2 | `max_connections` (400 dans l'image) : environ 300 postes simultanés | `LATTIS_PG_MAX_CONNECTIONS`, mémoire de PostgreSQL ; au-delà de quelques centaines de postes, jalon 8 de la spécification (§21) |
| Lectures lourdes (tableaux croisés, requêtes) | ponctuel | processeur et disque du parent | réplicas en lecture (`replicas.count`) : les postes y lisent tant qu'ils suivent (`LATTIS_REPLICA_MAX_LAG`) |
| Relais des changements (`/api/events`) | 1 flux HTTP ouvert | presque rien : une seule écoute PostgreSQL par `lattisd`, quel que soit le nombre de postes | plusieurs `lattisd` (`lattisd.replicaCount`) |
| `lattisd` | quelques requêtes par heure (jetons) | négligeable | 2 `lattisd` par défaut, pour les mises à jour sans interruption |

Le goulet de l'application de bureau est donc **le nombre de connexions au parent**, pas `lattisd`.

## Application web

Chaque personne connectée dans le navigateur a **son moteur** : un processus, un conteneur ou un pod `lattis-engine`, qui fait ce que fait l'application de bureau (cache local, synchronisation), côté serveur.

| Ressource | Par personne | Goulet | Ce qu'on règle |
|---|---|---|---|
| Mémoire | celle d'un moteur : de l'ordre de quelques centaines de Mo selon les classeurs ouverts (1 Gio réservé par pod dans le chart) | mémoire des nœuds | **Mémoire par session web** (limite), `web.resources.memory` |
| Processeur | faible au repos, pics au calcul | processeur des nœuds | `web.resources.cpu` |
| Connexions PostgreSQL | comme un poste de bureau | `max_connections` (partagé avec le bureau) | idem bureau |
| Démarrage | quelques secondes (pod : image à tirer, volume à monter) | attente à la connexion | réserve de moteurs prêts (`LATTIS_WEB_POOL_SPARE`) |
| Disque | cache par personne (volume) | stockage | `web.cache.size` |

Un moteur s'arrête après `LATTIS_WEB_IDLE` (30 min) sans onglet ouvert : la mémoire est rendue. Avec plusieurs `lattisd`, chaque `lattisd` porte ses moteurs ; une personne reste sur le même grâce à l'affinité par cookie.

Le goulet de l'application web est donc **la mémoire** : 100 personnes dans le navigateur, c'est de l'ordre de 100 moteurs, soit plusieurs dizaines de Go de mémoire.

## Limites réglables

Administration › Serveur › **Limites** (vide ou 0 : sans limite). Une limite atteinte affiche une page qui la nomme et dit quoi faire ; les sessions déjà ouvertes continuent, et les propriétaires du serveur entrent toujours.

| Limite | Effet |
|---|---|
| Utilisateurs connectés en même temps | Bureau et navigateur. Au-delà, une nouvelle connexion est refusée (« Serveur complet »). |
| Sessions de l'application web | Au-delà, plus de nouvelle session web, sous le plafond fixé à l'installation (`LATTIS_WEB_MAX_ENGINES`). |
| Mémoire totale de l'application web | Au-delà de cette mémoire occupée par les moteurs (mesurée comme sur l'écran Surveillance, toutes les 15 s), plus de nouvelle session web. Sans mesure possible (Kubernetes sans metrics-server), la limite est sans effet et le journal le signale. |
| Mémoire par session web | Mémoire allouée à chaque moteur ouvert ensuite. Kubernetes : limite du pod ; Docker : limite du conteneur, jamais au-delà de `LAUNCHER_MEMORY` ; mode processus : limite souple (`GOMEMLIMIT`), le moteur se cale dessous sans être arrêté. |

Pour la démo publique, ses propres plafonds s'appliquent en plus (`LATTIS_WEB_DEMO_MAX`, 2 par adresse).
