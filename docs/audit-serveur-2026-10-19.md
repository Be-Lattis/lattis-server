# Audit du Serveur Lattis — 19/10/2026

Audit en lecture seule du dépôt `lattis-server` au commit `f5054cd`, mené en deux volets parallèles puis fusionné :
- **Interface** : console `/admin`, pages de connexion et de premier mot de passe, lien `/w/<id>`, relais de l'édition web `/app/`, en-têtes HTTP ;
- **Fonctionnalités** : sécurité et droits, SQL et migrations, Data Lake Lattis, pipelines d'ingestion, édition web, exploitation et documentation.

Chaque point a été vérifié dans le code (chemins relatifs à la racine du dépôt). Les points sont classés par gravité, avec un correctif proposé. Les doublons entre les deux volets sont fusionnés. La dernière section ne garde que les vrais choix à trancher.

**En bref :** 5 critiques, 26 importants, 24 mineurs, 12 idées, 5 choix à valider. Les plus urgents :
- un changement d'adresse non vérifié au premier mot de passe peut capter des droits destinés à d'autres ;
- un publieur peut ouvrir des données qui ne sont pas à lui par l'audience d'un dossier ;
- les pipelines en ajout finissent par casser le tableau ;
- deux gestes de la console peuvent bloquer des personnes sans garde-fou.

---

## Critiques

### C1. Le changement d'adresse au premier mot de passe n'est pas vérifié, alors que des droits sont attribués par adresse
- **Constat** : `schema/0013_setup_email.sql:5-40` (voulu, à la demande de Louis le 18/10) laisse la personne qui tient un lien `/setup` remplacer l'adresse du compte par n'importe quelle adresse libre (`internal/auth/auth.go:564-567`). Or plusieurs mécanismes donnent des droits selon l'adresse :
  - `accept_invitations` fait la jointure `u.email = i.email`, à chaque connexion (`schema/0002_server_api.sql:141-149`, `:212-214`) ;
  - la connexion par mot de passe se déclare toujours `EmailVerified: true` (`internal/auth/auth.go:529`) ;
  - `invite` (`schema/0009_droits.sql:105-111`), `add_data_audience_by_email` (`schema/0019_audience_email.sql:15-16`) et `userByEmail` (`internal/admin/actions.go:345-353`) prennent « le compte le plus récemment connecté » qui porte l'adresse.
- **Impact** : une invitation (y compris de propriétaire du serveur restée en attente), un rôle de classeur ou une audience destinés à une adresse peuvent aboutir sur un compte qui s'est attribué cette adresse sans la prouver.
- **Correctif** :
  1. Ajouter `users.email_verified`, et le passer à faux quand l'adresse est changée au `/setup`.
  2. Faire passer cette valeur réelle à `enroll` et `accept_invitations` : une invitation ne s'applique qu'à une adresse vérifiée.
  3. Dans `invite`, `add_data_audience_by_email` et `userByEmail`, refuser une adresse portée par plusieurs comptes au lieu de prendre le dernier connecté.
  4. Voir « À valider » n°1.

### C2. Data Lake : tout publieur peut poser l'audience d'un dossier, donc ouvrir des données qui ne sont pas à lui
- **Constat** : pour un dossier, `add_data_audience`, `remove_data_audience`, `move_data_folder`, `rename_data_folder` et `drop_data_folder` ne demandent que `require_data_publisher()` (`schema/0018_datasets.sql:230`, `:508-512`, `:554-556`). `compile_dataset` ajoute l'audience des dossiers ancêtres à chaque donnée qui en hérite, ce qui est le cas par défaut (`0018:49`, `:314-318`). Gérer une donnée exige pourtant d'être propriétaire ou concepteur du classeur source (`can_manage_dataset`, `0018:108-113`). Le filtre d'une entrée de dossier s'exécute de plus dans une vue qui appartient au rôle propriétaire du classeur source (`0018:341-346`).
- **Impact** : un publieur sans rôle sur un classeur peut ouvrir la lecture de ses données, à lui-même ou à d'autres. Le contrat (§9) ne prévoit pas ce contournement.
- **Correctif** : donner un propriétaire aux dossiers (créateur et propriétaires du serveur). Pour poser une audience sur un dossier, le déplacer ou y ranger une donnée, exiger d'être propriétaire du serveur ou de pouvoir gérer toutes les données qu'il contient, sous-dossiers compris. Voir I12 pour les sous-requêtes dans les filtres, et « À valider » n°2.

### C3. Pipelines en ajout : la clé d'ordre `lattis_pos` s'allonge à chaque exécution jusqu'à casser l'insertion
- **Constat** : `internal/pipeline/write.go:91-105` part de `max(lattis_pos)` et y concatène `pad62(i+1, width) + "1"`. La plus grande clé grandit donc de 3 à 8 caractères par exécution. Or `lattis_pos` est indexée.
- **Impact** : un pipeline planifié toutes les 5 à 15 minutes produit, en quelques jours, des clés de plusieurs kilo-octets. L'insertion échoue alors sur la limite d'entrée d'un index btree (environ 2,7 Ko), et les tris ralentissent sur tous les postes.
- **Correctif** : générer les nouvelles clés au même niveau que la dernière (incrément borné, même algorithme que `internal/order` de l'appli) au lieu de concaténer la clé maximale. Ajouter un test de 1 000 exécutions qui vérifie la longueur maximale.

### C4. La console permet de désactiver le seul moyen de connexion, et personne ne peut plus entrer
- **Constat** : la case « État » est modifiable au crayon pour tous les fournisseurs, y compris « mot de passe » (`internal/admin/web/providers.html:18-19`). Ni `updateProvider` (`internal/admin/actions.go:246`) ni `update_provider` (`schema/0002_server_api.sql:280-310`) ne l'empêchent. Or seuls les fournisseurs actifs sont proposés à la connexion (`internal/store/store.go:157`, `:176`).
- **Impact** : une case décochée suffit pour bloquer tout le monde, propriétaires compris. La session en cours tient encore 8 h, donc rien ne prévient. Le retour à la normale passe par du SQL.
- **Correctif** : dans `update_provider`, refuser de désactiver le dernier fournisseur actif, et celui par lequel l'appelant s'est connecté, avec un message clair. Dans la console, exiger « Confirmer » avec le texte « Plus personne ne pourra se connecter par ce moyen ».

### C5. « Réinitialiser le mot de passe » agit en un clic, et le nouveau lien n'est montré qu'une fois
- **Constat** : le bouton n'a pas de confirmation, et il figure aussi sur la ligne de l'administrateur lui-même (`internal/admin/web/users.html:23`). `reset_password` efface le mot de passe et ferme toutes les sessions sur-le-champ (`schema/0002_server_api.sql:419-420`). Le nouveau lien n'apparaît que dans le message affiché une seule fois, sans bouton de copie (`internal/admin/actions.go:301`, `internal/admin/admin.go:508-519`). C'est aussi le cas du lien de création de compte (`actions.go:293`).
- **Impact** : un clic de trop coupe une personne de tous ses postes. Si le message est perdu, il faut réinitialiser à nouveau, ce qui invalide le lien précédent.
- **Correctif** : exiger « Confirmer » et masquer le bouton sur sa propre ligne. Afficher le lien dans un encadré avec un champ en lecture seule, un bouton « Copier » (dans `admin.js`) et la mention « Ce lien ne sera plus affiché : copiez-le maintenant ».

---

## Importants

### Sécurité et droits

**I1. Pipelines : les sources HTTP, SFTP et le webhook atteignent n'importe quelle adresse et suivent les redirections.**
- Constat : `fetchHTTP` accepte toute URL `http(s)://` et suit les redirections (`internal/pipeline/sources.go:131-171`). Le secret peut partir en clair sur `http://`, et un en-tête personnalisé reste attaché après une redirection vers un autre hôte. SFTP joint tout hôte et tout port (`sources.go:209-210`). Le webhook ne vérifie que le préfixe `https://` (`internal/pipeline/runner.go:317-336`).
- Impact : lattisd peut être utilisé pour interroger des services internes (métadonnées du nuage, PostgreSQL, `/metrics`), et la réponse est relisible dans un tableau.
- Correctif : un transport qui refuse le bouclage, les adresses privées, link-local et de métadonnées après résolution DNS, sauf liste blanche (`LATTIS_PIPELINE_ALLOWED_HOSTS`). Interdire les redirections qui changent d'hôte ou passent en `http`, et retirer les en-têtes d'authentification. Refuser `http://` avec un secret. Même filtre pour SFTP et le webhook. Voir « À valider » n°3.

**I2. Les pipelines lisent leurs secrets dans le dossier des clients OAuth.**
- Constat : `SecretsDir: cfg.SecretsDir` (`cmd/lattisd/main.go:228`), le même dossier que les fournisseurs (`main.go:177`). `secret_ref` peut nommer n'importe lequel de ses fichiers (`schema/0020_pipelines.sql:25`).
- Impact : un pipeline peut envoyer le secret d'un client OAuth vers l'hôte de sa source.
- Correctif : dossier dédié (`LATTIS_PIPELINE_SECRETS_DIR`, sans valeur par défaut commune) et préfixe obligatoire `pipeline-`.

**I3. Limitation de débit contournable derrière un mandataire.**
- Constat : avec `LATTIS_TRUST_PROXY=true`, l'adresse retenue est la première de `X-Forwarded-For`, c'est-à-dire celle que fournit le client (`internal/auth/auth.go:852-858`).
- Correctif : prendre l'adresse la plus à droite qui n'appartient pas à un mandataire de confiance (`LATTIS_TRUSTED_PROXIES` en CIDR). Limiter aussi `POST /login/{provider}` par adresse saisie.

**I4. Édition web : les moteurs tournent sous le même utilisateur système que lattisd.**
- Constat : `exec.Command` sans changement d'utilisateur ni bac à sable (`internal/webapp/webapp.go:205-223`). Le moteur peut donc lire la clé de signature, les secrets et les caches des autres utilisateurs. Son secret est passé par l'environnement.
- Impact : l'isolation par processus annoncée (spécification §14, §18) ne tient que si le moteur et les scripts web sont sans faille.
- Correctif : un UID dédié aux moteurs (au minimum différent de lattisd), clés et secrets lisibles par lattisd seul, secret passé par un descripteur hérité, `Pdeathsig` sous Linux. À terme, un conteneur ou un espace de noms par moteur.

**I5. Les filtres SQL (rôles, restrictions, audiences) acceptent sous-requêtes et appels de fonctions.**
- Constat : `check_predicate` refuse `;`, `$`, `\` et les commentaires, puis fait analyser l'expression, mais il accepte `FROM` et toute fonction (`schema/0005_advanced_rights.sql:23-76`). Dans une vue du Data Lake, les tables citées sont lues avec les droits du propriétaire de la vue (voir C2).
- Impact : un filtre peut s'appuyer sur des tables que son auteur ne peut pas lire, ou rendre un tableau très lent pour tous ses lecteurs.
- Correctif : n'accepter que les colonnes du tableau, les opérateurs, les littéraux et une liste de fonctions autorisées (`lattis.my_teams`, `current_user`, `now`, `lower`…). Au minimum, refuser `FROM` hors de `(SELECT lattis.my_teams())`.

### Pipelines

**I6. Un même pipeline peut tourner deux fois en même temps, et tous les pipelines passent en série.**
- Constat : la réservation `FOR UPDATE SKIP LOCKED` ne dure que la transaction de réservation (`internal/pipeline/runner.go:140-165`). Pendant l'exécution (jusqu'à 30 min), une autre instance ou `run_pipeline` (`schema/0020_pipelines.sql:206`) peut le relancer. Une instance n'exécute qu'un pipeline à la fois (`runner.go:115-123`).
- Impact : deux remplacements simultanés du même tableau, des doublons en mode ajout, des fichiers traités deux fois, et un pipeline lent qui retarde tous les autres.
- Correctif : verrou consultatif tenu pendant toute l'exécution (`pg_try_advisory_lock`) ou colonne `running_since`. Exécution parallèle avec un nombre borné.

**I7. Exécutions « en cours » orphelines, fichier envoyé écrasé, historique jamais purgé.**
- Constat : un arrêt de lattisd pendant l'exécution laisse `status = 'running'` pour toujours. Un second envoi écrase `pending_file` (`0020:206`). `pipeline_runs` n'est jamais purgé (`internal/store/oauth.go:125-134`).
- Correctif : au démarrage, passer en échec les exécutions « en cours » trop anciennes. Refuser un envoi tant qu'un fichier attend. Purger l'historique au-delà de N jours ou de N lignes par pipeline.

**I8. La mise à jour par clé joint sur `IS NOT DISTINCT FROM`, donc en coût quadratique.**
- Constat : le `MERGE` et le `DELETE … NOT EXISTS` du remplacement joignent sur `IS NOT DISTINCT FROM` (`internal/pipeline/write.go:141-182`), un opérateur qu'aucun index ni hachage ne peut servir.
- Impact : 100 000 × 100 000 comparaisons, et un dépassement de la limite de 30 min sur des volumes moyens.
- Correctif : rejeter les lignes dont une partie de la clé est vide, puis joindre avec `=`. Proposer un index (ou une contrainte unique) sur les colonnes clés à la création du pipeline.

**I9. Les horaires et les dates importées sont en UTC.**
- Constat : `next()` et `parseTime` utilisent `time.Local` (`runner.go:203-209`, `write.go:376`), mais l'image distroless n'a pas de base de fuseaux, donc `time.Local` vaut UTC même avec `TZ`. L'administration affiche aussi les dates en `Local` (`internal/admin/admin.go:129`).
- Impact : « chaque jour à 08:00 » tourne à 10:00 l'été à Paris, et les dates importées ou affichées sont décalées d'une à deux heures.
- Correctif : importer `time/tzdata`, et ajouter un réglage `timezone` du serveur (Europe/Paris par défaut) utilisé par la planification, la conversion et l'affichage.

**I10. Un pipeline peut occuper plusieurs Go de mémoire dans le processus qui gère les connexions.**
- Constat : la source est lue entièrement en mémoire (jusqu'à 100 Mo), puis découpée et convertie en tableaux de valeurs (jusqu'à 2 millions de lignes). En XLSX, chaque partie XML est décompressée jusqu'à 400 Mo (`internal/pipeline/sources.go:90`, `:166`, `:231`, `:442`).
- Impact : lattisd peut s'arrêter faute de mémoire, ce qui coupe aussi la connexion OAuth de tous les postes.
- Correctif : lecture en flux (CSV ligne à ligne, `CopyFrom` itératif), limites strictes de cellules et de taille décompressée, et à terme un processus séparé pour les pipelines.

**I11. L'envoi d'un fichier à un pipeline est coupé au bout de 30 s.**
- Constat : `ReadTimeout: 30 * time.Second` sur le serveur public (`cmd/lattisd/main.go:221-222`), que `uploadPipeline` ne lève pas (`internal/admin/pipelines.go:37-86`).
- Impact : un gros fichier échoue sur une liaison modeste, avec le message trompeur « Fichier trop gros ou formulaire expiré ».
- Correctif : `http.NewResponseController(w).SetReadDeadline(…15 min)` en tête de `uploadPipeline`, et un message distinct pour chacune des deux causes.

### Fonctionnel et spécification

**I12. Les opérations de structure du §9.3 n'existent pas.**
- Constat : seule `create_table` existe. Il n'y a ni `rename_table`, ni `drop_table`, ni `add_column`, `rename_column`, `drop_column`, `alter_column_type`, `set_key`, ni index (spécification §5.3 et §9.3).
- Impact : un classeur collaboratif publié ne peut pas changer de structure. Quand ces fonctions arriveront, les vues du Data Lake et les pipelines qui dépendent des noms de colonnes devront être recompilés dans la même transaction.
- Correctif : livrer ces fonctions avec cette recompilation, ou corriger la spécification pour dire qu'elles viendront plus tard.

**I13. « Durée d'une session » ne s'applique ni à la console ni à l'édition web.**
- Constat : le réglage ne régit que les jetons des postes (`internal/admin/pages.go:103-104`). La session navigateur est fixée à 8 h dans le code (`internal/admin/admin.go:44`).
- Impact : un propriétaire règle 30 jours, mais l'édition web déconnecte chaque jour, en pleine saisie (401 sur le WebSocket).
- Correctif : préciser le libellé (« application de bureau ») et mentionner les 8 h, ou rattacher la session navigateur à un réglage. Voir « À valider » n°4.

**I14. Pas de déconnexion ni de changement de compte hors de la console.**
- Constat : la page `/w/<id>` n'a pas de bouton de déconnexion (`internal/admin/links.go:563`), alors qu'en cas de refus elle invite à revenir « avec cette adresse » (`link.html:5-6`). `/admin/logout` exige le cookie anti-falsification limité à `/admin` (`admin.go:486`). Côté serveur, rien n'est prévu pour l'édition web.
- Correctif : une route `/logout` commune, et un bouton « Changer de compte » sur la page lien et sur les pages « Accès réservé » ou « Accès refusé ». Voir « À valider » n°5.

### Interface

**I15. Les champs masqués selon le contexte restent pris en compte.**
- Constat : `applyShowIf` ne fait qu'ajouter `hidden` et lit la valeur d'un champ même masqué (`internal/admin/web/admin.js:39-56`). Exemples :
  - « Séparateur » reste visible pour une source PostgreSQL, parce que « Format », masqué, vaut toujours `csv` (`pipelines.html:16`, `:28`) ;
  - « Nom de l'en-tête » reste visible après un passage de HTTP à SFTP (`:20`) ;
  - les valeurs masquées sont quand même envoyées (`auth=bearer`, colonnes clés en mode ajout).
- Correctif : ne tenir compte que des contrôles visibles, et désactiver ceux qui sont masqués sans que le crayon les réactive à l'envoi (`admin.js:24-26`). Côté serveur, ne garder dans `createPipeline` que les champs propres à la source choisie.

**I16. Les erreurs PostgreSQL brutes, en anglais, s'affichent telles quelles, et les gros formulaires perdent leur saisie.**
- Constat : `userMessage` affiche le message PostgreSQL (`internal/admin/admin.go:455-465`). Cas reproductibles :
  - pipeline en mode « mise à jour par clé » (le mode par défaut) sans colonne clé : violation de contrainte CHECK (`schema/0020_pipelines.sql:42`) ;
  - audience « Équipe » alors qu'il n'existe aucune équipe : identifiant vide (`data.html:9`) ;
  - restriction ou « Voir en tant que… » sur un classeur sans tableau (`workbook.html:47`, `:72`) ;
  - filtre SQL mal écrit.
  Après une erreur, la redirection vide le formulaire (`actions.go:159`).
- Correctif : valider côté Go avant l'appel SQL, avec des messages en français. Ne pas proposer d'options sans choix possible. Traduire les codes PostgreSQL courants (23514, 22P02, 42601). Réafficher le formulaire avec la saisie après une erreur.

**I17. Les pages d'erreur et de message sont des impasses.**
- Constat : `admin.message` ne passe pas l'utilisateur à la vue, donc ni menu ni déconnexion ne s'affichent (`internal/admin/admin.go:440-446`). C'est le cas pour « Introuvable », « Demande refusée », « Accès réservé », « Erreur » et même « Déconnecté ». Les pages « Session expirée », « Accès refusé » et « Mot de passe enregistré » n'ont pas de lien non plus (`internal/auth/auth.go:393`, `:618`, `:783`). « Jeton de formulaire invalide » est un message technique (`admin.go:319`).
- Correctif : un lien de retour sur chaque page de message (« Retour à l'administration », « Se reconnecter », « Revenir à la page »). Reformuler : « Cette page était restée ouverte trop longtemps. Revenez-y et recommencez. »

**I18. Les messages de connexion disent « depuis Lattis » même dans le navigateur.**
- Constat : `expired` et `loginError` renvoient vers l'application (`internal/auth/auth.go:403-404`, `:783-784`), y compris pour la console, `/w/` et `/app/`.
- Correctif : pour le client `lattis-admin`, proposer un lien « Recommencer » qui conserve la page demandée.

**I19. Après le premier mot de passe, la personne ne sait pas où aller.**
- Constat : un non-propriétaire lit seulement « Vous pouvez maintenant vous connecter… depuis Lattis » (`internal/auth/auth.go:618-619`).
- Correctif : afficher l'adresse du serveur à saisir dans l'appli, un bouton « Ouvrir dans Lattis », un lien « Télécharger Lattis » (`LATTIS_DOWNLOAD_URL`) et, en édition web, « Ouvrir dans le navigateur ».

**I20. L'adresse racine du serveur affiche du JSON brut.**
- Constat : `GET /` redirige vers `/.well-known/lattis.json` (`internal/httpserver/server.go:48-50`).
- Correctif : servir une page d'accueil (nom et logo du serveur, liens vers l'édition web, l'administration et le téléchargement). Voir « À valider » n°5.

**I21. Les contrôles « créateur de classeurs » s'affichent même quand tout le monde peut publier.**
- Constat : quand le réglage vaut « Tout utilisateur », la case, les boutons et l'étiquette restent affichés (`internal/admin/web/users.html:8`, `:18`, `:24-25`).
- Correctif : lire `workbook_creators` dans `loadUsers` et n'afficher ces contrôles que pour la valeur « Propriétaires et créateurs ».

**I22. La page Utilisateurs est trop chargée et déborde sur mobile.**
- Constat : une ligne peut porter jusqu'à quatre formulaires (`users.html:22-32`). Les tableaux Utilisateurs, Invitations, Journal et Tenants ne défilent pas horizontalement (`users.html:42`, `audit.html:8`, `providers.html:24`).
- Correctif : envelopper ces tableaux dans un conteneur défilant. Regrouper par utilisateur les actions dans un bloc repliable « Gérer » (rôles au crayon, mot de passe, désactivation avec confirmation).

**I23. Des champs n'ont pas de libellé : le texte indicatif sert d'étiquette.**
- Constat : pas de `<label>` ni d'`aria-label` sur les filtres du Journal (`audit.html:3-5`), l'ajout d'utilisateur et la recherche (`users.html:6`, `:12`), l'ajout de membre (`teams.html`), les formulaires d'un classeur (`workbook.html:17`, `:36`, `:61`, `:71-72`) ni le rôle par défaut d'un tenant (`providers.html:32`). « Confirmer », « Supprimer » et « Retirer » ne disent pas à quoi ils s'appliquent.
- Correctif : `aria-label` explicites, et un nom passé au gabarit `confirm` (« Confirmer la suppression : <classeur> »).

**I24. Sur la page d'un classeur, l'essentiel est en bas.**
- Constat : « Inviter à ce classeur » vient après tous les tableaux de niveaux de tous les rôles (`workbook.html:9-31`, `:58-66`). Le nom du classeur n'est pas le titre, et le schéma technique s'affiche dès le haut.
- Correctif : le nom du classeur en titre, « Inviter » juste sous le lien, les niveaux de chaque rôle repliés, et les détails techniques (schéma, écarts) dans une section repliée.

**I25. Les niveaux affichent « Feuille <identifiant> » au lieu du nom de la feuille.**
- Constat : `"Feuille " + sid` (`internal/admin/pages.go:359`). Le nom des feuilles n'est pas publié dans `lattis_meta_tables`.
- Correctif : publier le nom des feuilles (colonne `sheet_name`). À défaut, afficher « Feuille 1, 2… » dans l'ordre.

**I26. Retirer un tenant se fait sans confirmation.**
- Constat : `providers.html:26`, et `exec1(…, false, …)` (`internal/admin/actions.go:60`). Sans tenant, plus personne n'entre par ce fournisseur.
- Correctif : exiger « Confirmer », avec un avertissement quand c'est le dernier tenant.

---

## Mineurs

**Sécurité et exploitation**
- **M1. Énumération des comptes par le message de verrouillage.** « Compte verrouillé » ne s'affiche que pour un compte existant (`internal/auth/auth.go:511-514`). Correctif : garder le message uniforme pendant le verrouillage.
- **M2. Une session d'administration ne se révoque pas avant 8 h.** Le cookie est un jeton signé sans état, et la déconnexion ne l'invalide pas (`internal/admin/admin.go:302-326`). Correctif : identifiant de session enregistré en base et révoqué à la déconnexion.
- **M3. `/engine/token` est servi sur l'écoute publique.** Il n'est protégé que par l'adresse de bouclage et le secret (`internal/webapp/webapp.go:313-316`). Correctif : une écoute séparée sur 127.0.0.1 ou une socket Unix.
- **M4. `dataset_changed_ids` n'est pas borné.** Avec un `since` ancien, la fonction parcourt tout le tableau, et elle révèle le rythme de modification de lignes invisibles (`schema/0021_dataset_sync.sql:24-41`). Correctif : pagination, ou refus d'un `since` trop ancien.
- **M5. Le contrat client est incohérent sur la borne de `since`.** Il écrit `lattis_xid > $since` pour les lignes changées (`docs/contrat-client.md:153`) alors que tout le reste utilise `>=`. Correctif : `>=` partout.
- **M6. `compile_dataset` ne prévient pas les clients quand la source n'est plus publiée.** Il sort sans `pg_notify` (`schema/0018_datasets.sql:288-290`). Correctif : notifier avant de sortir.
- **M7. Les CSV Windows-1252 sont mal décodés.** Le décodage octet par octet en Latin-1 transforme `€`, `’` et `œ` en caractères de contrôle (`internal/pipeline/sources.go:300-307`). Correctif : décoder en Windows-1252.
- **M8. Tout nombre dans une colonne date est pris pour un numéro de série Excel.** « 12 » devient une date de 1900 (`internal/pipeline/write.go:364-367`). Correctif : accepter les numéros de série seulement pour une source XLSX, ou au-delà d'un seuil.
- **M9. Un pipeline « fichier envoyé » avec un horaire échoue à chaque échéance**, et déclenche l'alerte à chaque fois (`sources.go:61-64`). Correctif : contrainte qui limite ce type de source à « À la demande ».
- **M10. Le cache de l'édition web n'est jamais nettoyé.** Il reste après la désactivation d'un compte (`webapp.go:201-203`). Correctif : le supprimer avec le compte, et purger les dossiers inutilisés.
- **M11. L'exemple Compose ouvre PostgreSQL et `lattis_service` à tout le réseau.** Il publie le port `5432` et accepte ces rôles depuis `0.0.0.0/0` (`deploy/compose/docker-compose.yml`, `deploy/postgres/initdb/10-lattis.sh`). Correctif : dire dans `installation.md` de les restreindre en production.

**Interface**
- **M12. Le vocabulaire du Data Lake est encore mélangé.** On lit « publieur de données » et « peut maintenant partager des données » (`users.html:18`, `:26-27`, `actions.go:70-71`). Correctif : « publieur au Data Lake Lattis ».
- **M13. Le contraste est insuffisant en mode sombre.** Le texte blanc sur le vert `#3fa36b` atteint environ 3,2:1, sous le seuil AA de 4,5:1 (`admin.css:2`, `lattis.css:2`). Correctif : un fond de bouton plus foncé (`#2a7d4f`).
- **M14. « Tout déplier » et « Tout replier » restent affichés sans JavaScript** (`teams.html`, `data.html`). Correctif : n'afficher ces boutons que si le script est chargé.
- **M15. Le crayon ne permet pas d'annuler.** Correctif : transformer le crayon en « Annuler », qui restaure la valeur et rebloque le champ.
- **M16. Un champ vide d'une section repliée peut faire échouer l'enregistrement d'un autre réglage du serveur** (`admin.js:24-26`, `actions.go:192-193`). Correctif : ne valider que les champs modifiés.
- **M17. Le réglage « Scripts dans l'application web » s'affiche hors édition web** (`pages.go:111`). Correctif : le masquer quand l'édition n'est pas `web`.
- **M18. Dans les blocs repliables, le titre perd son niveau de titre** (`<summary><h2>`). C'est acceptable. Pour corriger, placer le titre hors du `summary`.
- **M19. Sur `/w/`, le choix « navigateur » est difficile à changer, et rien n'aide si Lattis n'est pas installé** (`links.go:601-603`, `link.html:11`, `:17`). Correctif : un lien « changer » accessible depuis l'édition web, et toujours une solution de repli.
- **M20. Les pages de message de la console affichent « Serveur Lattis » au lieu du nom configuré, et le pied de page n'a pas de version** (`admin.go:441`).
- **M21. La liste des utilisateurs est limitée à 500 sans le dire** (`pages.go:231`). Correctif : afficher « 500 premiers résultats : affinez la recherche ».
- **M22. La page de premier mot de passe manque d'aide.** Le champ adresse n'a pas d'explication, « Confirmation » est vague, et les messages d'erreur commencent par une minuscule (`setup.html:6`, `:8`, `password/argon2.go:29`).
- **M23. Le logo n'est pas contrôlé.** Une adresse `http://` est acceptée puis bloquée par la politique de sécurité des pages, qui n'autorise que `https:`, et le logo s'affiche cassé sans explication (`actions.go:199-200`). Correctif : refuser ce qui ne commence pas par `https://`.
- **M24. Plusieurs messages sont ambigus, et il reste du code mort.**
  - L'envoi de fichier affiche « Fichier trop gros ou formulaire expiré » pour deux causes différentes (`pipelines.go:590-591`).
  - « Invitez-le d'abord » apparaît sur la page Équipes, où aucune invitation n'est possible (`actions.go:350`).
  - « Voir en tant que… » sans tableau ne dit rien.
  - Il reste un `else {}` vide et des classes CSS `.team*` qui ne servent plus.

---

## Idées

- **Mesures :** durée et nombre de lignes par exécution de pipeline, âge de la plus ancienne exécution « en cours », données du Data Lake dont la source est indisponible.
- **Historique d'audience :** journaliser l'audience effective appliquée par `compile_dataset`, pour pouvoir répondre à « qui pouvait lire cette donnée à telle date ».
- **Test de mise à jour en CI :** partir d'une base à la migration N-1, avec des classeurs, des données et des pipelines, passer `--migrate`, puis vérifier la recompilation des vues et `check_drift`.
- **Simulation d'un pipeline :** lecture et conversion avec le rapport des rejets, sans rien écrire.
- **Copier le lien d'un classeur :** un bouton « Copier » à côté du lien à partager.
- **Suppression d'un classeur plus sûre :** faire retaper son titre avant de le supprimer.
- **Boutons destructeurs :** les laisser inactifs tant que « Confirmer » n'est pas coché (avec JavaScript).
- **Menu mobile :** sous 640 px, ranger les neuf onglets de la console dans un menu repliable.
- **Chemin complet des équipes :** dans « Ajouter l'équipe » d'un rôle, afficher « Ventes › Nord » pour distinguer deux sous-équipes homonymes.
- **Mot de passe oublié :** un texte « Demandez un nouveau lien à l'administrateur du serveur » sous le formulaire de connexion.
- **Expiration des liens :** l'indiquer pour les comptes en attente.
- **« Voir en tant que… » en POST :** l'adresse de la personne ne passerait plus dans l'URL.

---

## À valider

1. **Changer d'adresse au premier mot de passe (C1).** C'est toi qui l'as demandé le 18/10. Garde-t-on la possibilité, en marquant la nouvelle adresse « non vérifiée », sans qu'elle reçoive d'invitations ni d'audiences tant qu'elle n'est pas confirmée ? Cette confirmation demanderait un envoi d'e-mails que le serveur n'a pas aujourd'hui. Ou bien fige-t-on l'adresse prévue par l'administrateur ? *Recommandation :* figer l'adresse tant qu'il n'y a pas d'envoi d'e-mails ; l'administrateur corrige une adresse fausse en recréant le compte.
2. **Qui gère les dossiers du Data Lake (C2).** Les publieurs administrent-ils tout le catalogue, ce qui serait alors un choix à écrire dans le contrat, ou chacun n'agit-il que sur ses propres données ? *Recommandation :* chacun sur ses données, les dossiers étant gérés par leur créateur et par les propriétaires du serveur.
3. **Pipelines et réseau interne (I1).** Le propriétaire du serveur est-il de confiance pour le réseau interne de l'hébergement ? Si oui, une liste d'hôtes autorisés réglée par l'exploitant suffit. Sinon, les adresses privées doivent être bloquées par défaut. *Recommandation :* bloquer par défaut, avec une liste blanche réglable.
4. **Durée de session dans le navigateur (I13).** Garde-t-on 8 h fixes pour la console et l'édition web, ou un réglage dédié ? *Recommandation :* un réglage « session navigateur » (8 h par défaut), renouvelé tant qu'on s'en sert.
5. **Accueil et déconnexion web (I14, I20).** Que montrer à la racine `/` : une page d'accueil publique avec des liens, ou une redirection vers `/app/` en édition web et vers `/admin/` sinon ? Et l'appli web a-t-elle déjà un « Se déconnecter », et vers quelle route ? *Recommandation :* une page d'accueil avec des liens, et une route `/logout` commune que l'appli web appelle.
