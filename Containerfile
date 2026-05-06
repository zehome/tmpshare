# syntax=docker/dockerfile:1

# ---------- Stage 1 : bundle JS via esbuild ----------
FROM docker.io/library/node:22-alpine AS web
WORKDIR /src
COPY package.json package-lock.json* ./
RUN npm install --silent --no-audit --no-fund
COPY web ./web
RUN npm run build

# ---------- Stage 2 : binaire Go ----------
FROM docker.io/library/golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
COPY --from=web /src/web ./web
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/tmpshare .

# ---------- Stage 3 : runtime ----------
FROM scratch
# Root CAs — sans ça, autocert ne peut pas vérifier le cert TLS de Let's Encrypt.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/tmpshare /tmpshare

ENV LISTEN=":8080" \
    DATA_DIR="/data" \
    BASE_URL="http://localhost:8080" \
    DEFAULT_EXPIRES="168h" \
    MAX_UPLOAD_SIZE="0" \
    HTTP3="1"
# UPLOAD_TOKEN doit être fourni au runtime (-e UPLOAD_TOKEN=...)
# Pour activer le listener TLS direct + Let's Encrypt, fournir au runtime :
#   TLS_LISTEN=":444" TLS_DOMAINS="exemple.com" TLS_ACME_EMAIL="…"
# HTTP/3 (QUIC) est activé par défaut — il ne s'allume que si TLS_LISTEN est défini.

VOLUME ["/data"]
EXPOSE 8080 444/tcp 444/udp

ENTRYPOINT ["/tmpshare"]
