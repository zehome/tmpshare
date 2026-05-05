---
name: update-deps
description: Mettre à jour les dépendances du projet tmpshare (Go modules + npm) vers leurs dernières versions, puis rebuild et vérifier que tout compile. Utiliser quand l'utilisateur demande de "mettre à jour les dépendances", "vérifier les versions", "maj des deps", "update deps", ou équivalent.
---

# update-deps

Met à jour les dépendances du projet tmpshare (binaire Go + bundle JS) vers leurs dernières versions, vérifie qu'elles compilent, et rapporte les changements.

## Périmètre

Le projet a deux gestionnaires de dépendances :

- **Go** : `go.mod` / `go.sum` — module `tmpshare`, dépendance directe `github.com/tus/tusd/v2`
- **npm** : `package.json` / `package-lock.json` — bundle `web/src/app.js` → `web/vendor/app.js` via esbuild
  - dependencies : `preact`, `htm`, `tus-js-client`
  - devDependencies : `esbuild`

Le binaire Go embarque `web/vendor/app.js` via `//go:embed`, donc toute maj npm impose un `go build` derrière.

## Procédure

### 1. État des lieux

Lance en parallèle :

```bash
go list -m -u all 2>&1 | grep -E '\[' | head -40
npm outdated
```

Identifie :
- les deps **directes** Go avec maj disponible (les `[vX.Y.Z]` à droite). Une dep est directe si elle apparaît dans `go.mod` sous `require (...)` sans `// indirect`.
- les deps npm avec colonne `Latest` > `Current`.

Les indirectes Go ne sont pas gérées manuellement — `go mod tidy` les ajustera.

### 2. Rapporter avant d'agir

Présente un récap court à l'utilisateur :

```
Go (directes) :
  - github.com/tus/tusd/v2  v2.9.2 → v2.10.0  (mineur)

npm :
  - preact   10.29.1 → 10.30.0  (patch)
  - esbuild   0.28.0 → 0.29.0   (mineur, devDep)
```

Signale les majs **majeures** (ex: `preact 10.x → 11.x`, `esbuild 0.x → 1.x`) qui peuvent casser le build, et demande confirmation avant de les appliquer. Les patch/mineures peuvent être appliquées sans demander.

### 3. Appliquer

**npm** — toujours via les commandes `npm install` (jamais éditer `package.json` à la main) pour que `package-lock.json` reste cohérent :

```bash
npm install --save preact@latest htm@latest tus-js-client@latest
npm install --save-dev esbuild@latest
```

**Go** — directes uniquement, puis tidy :

```bash
go get -u github.com/tus/tusd/v2@latest
# (autres directes si applicable, listées par go list -m -u all)
go mod tidy
```

Pour mettre aussi à jour les indirectes transitives (rare, en général inutile) : `go get -u ./...` puis `go mod tidy`.

### 4. Rebuild + vérif

```bash
make build
```

Cela enchaîne `npm run build` (esbuild → `web/vendor/app.js`) puis `go build -o tmpshare`. Les deux doivent réussir. Si `make build` échoue :

- Erreur esbuild : probable breaking change d'esbuild (flags supprimés/renommés). Lire le message, ajuster `package.json` (script `build`) ou le code.
- Erreur Go : breaking API dans tusd. Lire le diff sur https://github.com/tus/tusd/releases, ajuster `main.go` (notamment `setupTus`, `finalizeTus`).
- Erreur runtime preact/tus-js-client : tester en local avec `./tmpshare` et la page web (upload + liste + delete).

En cas de breaking change non trivial, **revenir à la version précédente** plutôt que bricoler à l'aveugle :

```bash
git checkout -- package.json package-lock.json go.mod go.sum   # si suivi par git
# ou réinstaller la version précédente explicitement
npm install --save preact@10.29.1
```

### 5. Audit sécurité npm

```bash
npm audit
```

Si vulnérabilités présentes : ne jamais lancer `npm audit fix --force` aveuglément. Lire le rapport, juger si la vuln touche réellement le bundle (la plupart du temps non, car les vulns sont dans des outils dev), et n'agir que si pertinent.

### 6. Test fonctionnel rapide

Si possible, lancer le binaire et vérifier que la page web charge sans erreur console :

```bash
./tmpshare &
sleep 1
curl -sf http://localhost:8080/ > /dev/null && echo "OK" || echo "KO"
kill %1 2>/dev/null
```

(Adapte si `UPLOAD_TOKEN` est requis dans l'env de l'utilisateur.)

### 7. Rapport final

Récapitule :
- les versions avant → après pour chaque dep mise à jour
- la taille du bundle avant/après si elle a notablement changé (`web/vendor/app.js`)
- toute alerte (vulnérabilité, breaking change évité, dep restée en arrière)

## Notes spécifiques au projet

- **`go.sum` doit toujours être committé** après `go mod tidy`.
- **`package-lock.json` doit toujours être committé** après `npm install`.
- Le fichier `web/vendor/app.js` est embarqué dans le binaire — sans `make build` (ou au moins `npm run build` + `go build`), les changements JS ne sont pas pris en compte au runtime.
- `tusd v2.x` a une API stable mais surveille les changements dans `pkg/handler` (`PreFinishResponseCallback`, `tushandler.Config`) et `pkg/filestore` à chaque maj mineure.
- Les versions des deps `preact` et `htm` doivent rester compatibles : `htm` n'a pas de dépendance directe à preact, mais utilise son `h` runtime.
