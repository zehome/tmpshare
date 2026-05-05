# tmpshare

Partage temporaire de fichiers. Upload via `curl`, lien public en retour.

## Installation (une fois)

Ajoute dans ton `~/.bashrc` ou `~/.zshrc` :

```sh
export TMPSHARE_URL="https://tmpshare.example.com"
export TMPSHARE_TOKEN="…"

tmpshare() {
  curl -T "$1" -H "X-Auth-Token: $TMPSHARE_TOKEN" \
    "$TMPSHARE_URL/u/$(basename "$1")"
}
```

## Utilisation

```sh
tmpshare rapport.pdf
```

Renvoie un JSON contenant `url` (lien court) et `url_named` (lien avec
le nom du fichier).

## Durée d'expiration

Ajoute `?expires=30m` (`2h`, `24h`, `168h`…) à l'URL :

```sh
curl -T secret.txt -H "X-Auth-Token: $TMPSHARE_TOKEN" \
  "$TMPSHARE_URL/u/secret.txt?expires=30m"
```
