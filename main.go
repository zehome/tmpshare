package main

import (
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/quic-go/quic-go/http3"
	"github.com/tus/tusd/v2/pkg/filestore"
	tushandler "github.com/tus/tusd/v2/pkg/handler"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/text/unicode/norm"
)

// Noms de périphériques Windows réservés — préfixés d'un "_" si rencontrés,
// pour qu'un téléchargement via wget --content-disposition n'échoue pas côté Windows.
var windowsDeviceFiles = func() map[string]bool {
	m := map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}
	for i := 0; i < 10; i++ {
		m[fmt.Sprintf("COM%d", i)] = true
		m[fmt.Sprintf("LPT%d", i)] = true
	}
	return m
}()

//go:embed web
var webFS embed.FS

type meta struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

type uploadResponse struct {
	Success   bool      `json:"success"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	URL       string    `json:"url"`       // courte : {base}/{key}
	URLNamed  string    `json:"url_named"` // {base}/{key}/{name}, marche avec wget par défaut
	ExpiresAt time.Time `json:"expires_at"`
}

var (
	dataDir        string
	tusDir         string
	baseURL        string
	defaultExpires time.Duration
	maxUploadSize  int64
	uploadToken    string

	tlsListen       string
	tlsDomains      []string
	tlsCacheDir     string
	tlsRedirectPort string
	tlsACMEEmail    string
	http3Enabled    bool

	// Injecté au build via -ldflags "-X main.buildVersion=...".
	buildVersion = "dev"

	mu sync.Mutex
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// authed enrobe un handler avec checkAuth ; renvoie 401 si refusé.
func authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func authStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// checkAuth — comparaison à temps constant.
func checkAuth(r *http.Request) bool {
	got := r.Header.Get("X-Auth-Token")
	if got == "" {
		if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
			got = strings.TrimPrefix(h, "Bearer ")
		}
	}
	if got == "" {
		got = r.URL.Query().Get("token")
	}
	if len(got) != len(uploadToken) {
		return false
	}
	var diff byte
	for i := 0; i < len(got); i++ {
		diff |= got[i] ^ uploadToken[i]
	}
	return diff == 0
}

func main() {
	dataDir = envOr("DATA_DIR", "./data")
	tusDir = filepath.Join(dataDir, "tus")
	baseURL = strings.TrimRight(envOr("BASE_URL", "http://localhost:8080"), "/")
	listen := envOr("LISTEN", ":8080")
	uploadToken = os.Getenv("UPLOAD_TOKEN")
	if uploadToken == "" {
		log.Fatal("UPLOAD_TOKEN est obligatoire")
	}

	d, err := time.ParseDuration(envOr("DEFAULT_EXPIRES", "168h"))
	if err != nil {
		log.Fatalf("DEFAULT_EXPIRES invalide: %v", err)
	}
	defaultExpires = d

	maxUploadSize = 0 // 0 = illimité
	if v := os.Getenv("MAX_UPLOAD_SIZE"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &maxUploadSize); err != nil {
			log.Fatalf("MAX_UPLOAD_SIZE invalide: %v", err)
		}
	}

	tlsListen = os.Getenv("TLS_LISTEN")
	if d := os.Getenv("TLS_DOMAINS"); d != "" {
		for _, s := range strings.Split(d, ",") {
			if s = strings.TrimSpace(s); s != "" {
				tlsDomains = append(tlsDomains, s)
			}
		}
	}
	tlsCacheDir = envOr("TLS_CACHE_DIR", filepath.Join(dataDir, "autocert"))
	tlsACMEEmail = os.Getenv("TLS_ACME_EMAIL")
	tlsRedirectPort = os.Getenv("TLS_REDIRECT_PORT")
	if tlsRedirectPort == "" && tlsListen != "" {
		if _, p, err := net.SplitHostPort(tlsListen); err == nil {
			tlsRedirectPort = p
		}
	}
	switch strings.ToLower(os.Getenv("HTTP3")) {
	case "1", "true", "yes", "on":
		http3Enabled = true
	}

	if err := os.MkdirAll(tusDir, 0o755); err != nil {
		log.Fatalf("création %s: %v", tusDir, err)
	}

	tusH, err := setupTus()
	if err != nil {
		log.Fatalf("tusd setup: %v", err)
	}

	go janitor()

	mux := http.NewServeMux()
	mux.HandleFunc("/", root)
	mux.HandleFunc("/favicon.svg", serveFavicon)
	mux.HandleFunc("/vendor/", serveVendor)
	mux.HandleFunc("/u/", authed(upload))
	mux.HandleFunc("/list", authed(list))
	mux.HandleFunc("/finalize/", authed(finalize))
	mux.HandleFunc("/api/", authed(apiDelete))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"version": buildVersion})
	})
	mux.HandleFunc("/auth", authed(authStatus))
	mux.Handle("/files/", http.StripPrefix("/files", authedTus(tusH)))

	if tlsListen != "" {
		if len(tlsDomains) == 0 {
			log.Fatal("TLS_LISTEN défini mais TLS_DOMAINS manquant (liste de domaines autorisés pour autocert)")
		}
		if err := os.MkdirAll(tlsCacheDir, 0o700); err != nil {
			log.Fatalf("création %s: %v", tlsCacheDir, err)
		}
		serveTLS(mux, listen)
		return
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	log.Printf("tmpshare en écoute sur %s — data=%s base=%s default-expires=%s",
		listen, dataDir, baseURL, defaultExpires)
	log.Fatal(srv.ListenAndServe())
}

// serveTLS lance le listener HTTPS direct (avec autocert + Let's Encrypt) sur
// tlsListen, optionnellement HTTP/3 sur le même endpoint UDP, et un listener
// plain HTTP sur listen qui répond aux ACME HTTP-01 et redirige tout le reste
// en 308 vers https://host:tlsRedirectPort/...
//
// L'idée : le reverse proxy fronte le port public 443 (avec ses propres certs)
// et forwarde vers ce listener plain. Ce listener émet alors un redirect
// permanent vers le port direct (ex. 444) où ce service gère ses propres certs
// via Let's Encrypt et expose HTTP/2 + HTTP/3 — éliminant le mixed content
// causé par le proxying.
func serveTLS(mux http.Handler, listen string) {
	m := &autocert.Manager{
		Cache:      autocert.DirCache(tlsCacheDir),
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(tlsDomains...),
		Email:      tlsACMEEmail,
	}

	tlsCfg := m.TLSConfig()
	tlsCfg.MinVersion = tls.VersionTLS12
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)

	tlsHandler := mux
	if http3Enabled && tlsRedirectPort != "" {
		tlsHandler = withAltSvc(mux, tlsRedirectPort)
	}

	tlsSrv := &http.Server{
		Addr:              tlsListen,
		Handler:           tlsHandler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 30 * time.Second,
	}

	plainSrv := &http.Server{
		Addr:              listen,
		Handler:           m.HTTPHandler(redirectToTLSHandler(tlsRedirectPort)),
		ReadHeaderTimeout: 30 * time.Second,
	}

	go func() {
		log.Printf("tmpshare HTTP redirect→HTTPS (+ACME HTTP-01) sur %s", listen)
		if err := plainSrv.ListenAndServe(); err != nil {
			log.Fatalf("plain http: %v", err)
		}
	}()

	if http3Enabled {
		h3 := &http3.Server{
			Addr:      tlsListen,
			Handler:   tlsHandler,
			TLSConfig: http3.ConfigureTLSConfig(tlsCfg),
		}
		go func() {
			log.Printf("tmpshare HTTP/3 (QUIC) sur %s/udp", tlsListen)
			if err := h3.ListenAndServe(); err != nil {
				log.Printf("http3: %v", err)
			}
		}()
	}

	log.Printf("tmpshare HTTPS sur %s — domaines=%v http3=%v", tlsListen, tlsDomains, http3Enabled)
	log.Fatal(tlsSrv.ListenAndServeTLS("", ""))
}

func redirectToTLSHandler(port string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil && h != "" {
			host = h
		}
		target := "https://" + host
		if port != "" && port != "443" {
			target += ":" + port
		}
		target += r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusPermanentRedirect)
	})
}

// withAltSvc annonce HTTP/3 via l'entête Alt-Svc — les navigateurs
// compatibles basculeront sur QUIC pour les requêtes suivantes.
func withAltSvc(h http.Handler, port string) http.Handler {
	altSvc := fmt.Sprintf(`h3=":%s"; ma=2592000`, port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", altSvc)
		h.ServeHTTP(w, r)
	})
}


// ----- routing racine -----

func root(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case p == "/" && r.Method == http.MethodGet:
		serveIndex(w, r)
	case p == "/" && r.Method == http.MethodPost:
		authed(uploadMultipart)(w, r)
	case strings.HasPrefix(p, "/static/"):
		serveStatic(w, r)
	default:
		// /{key} ou /{key}/{nom} (le nom est purement décoratif pour wget).
		rest := strings.TrimPrefix(p, "/")
		key, _, _ := strings.Cut(rest, "/")
		if !validKey(key) {
			http.NotFound(w, r)
			return
		}
		download(w, r, key)
	}
}

func serveIndex(w http.ResponseWriter, _ *http.Request) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func serveStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	http.StripPrefix("/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func serveFavicon(w http.ResponseWriter, r *http.Request) {
	b, err := webFS.ReadFile("web/favicon.svg")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(b)
}

func serveVendor(w http.ResponseWriter, r *http.Request) {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

// ----- upload PUT (body brut) -----

func upload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	name := sanitizeName(strings.TrimPrefix(r.URL.Path, "/u/"))
	if name == "" {
		http.Error(w, "nom de fichier manquant — utilise PUT /u/{nom}", http.StatusBadRequest)
		return
	}

	expires, err := parseExpires(r.URL.Query().Get("expires"))
	if err != nil {
		http.Error(w, "expires invalide: "+err.Error(), http.StatusBadRequest)
		return
	}

	body := io.Reader(r.Body)
	if maxUploadSize > 0 {
		body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	}
	defer r.Body.Close()

	ct := r.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}

	resp, err := store(name, ct, body, expires)
	if err != nil {
		log.Printf("upload error: %v", err)
		http.Error(w, "upload échoué: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----- upload POST multipart (legacy / petits fichiers) -----

func uploadMultipart(w http.ResponseWriter, r *http.Request) {
	if maxUploadSize > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "multipart invalide: "+err.Error(), http.StatusBadRequest)
		return
	}
	f, fh, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "champ 'file' manquant", http.StatusBadRequest)
		return
	}
	defer f.Close()

	name := r.FormValue("name")
	if name == "" {
		name = fh.Filename
	}
	name = sanitizeName(name)
	if name == "" {
		http.Error(w, "nom de fichier vide", http.StatusBadRequest)
		return
	}
	expires, err := parseExpires(r.FormValue("expires"))
	if err != nil {
		http.Error(w, "expires invalide: "+err.Error(), http.StatusBadRequest)
		return
	}
	ct := fh.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/octet-stream"
	}
	resp, err := store(name, ct, f, expires)
	if err != nil {
		log.Printf("upload error: %v", err)
		http.Error(w, "upload échoué: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// ----- TUS (resumable) -----

func setupTus() (http.Handler, error) {
	store := filestore.New(tusDir)
	composer := tushandler.NewStoreComposer()
	store.UseIn(composer)

	cfg := tushandler.Config{
		BasePath:                "/files/",
		StoreComposer:           composer,
		MaxSize:                 maxUploadSize,
		RespectForwardedHeaders: true,
		PreFinishResponseCallback: func(evt tushandler.HookEvent) (tushandler.HTTPResponse, error) {
			return finalizeTus(evt.Upload)
		},
	}
	return tushandler.NewHandler(cfg)
}

func authedTus(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Laisser passer le preflight OPTIONS — tusd répond aux entêtes CORS lui-même.
		if r.Method != http.MethodOptions && !checkAuth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// finalizeTus est appelé synchrone par tusd avant la réponse 204 finale.
// Il renomme le fichier tusd, écrit le meta.json et un mapping done_{id}.json.
func finalizeTus(info tushandler.FileInfo) (tushandler.HTTPResponse, error) {
	name := sanitizeName(info.MetaData["filename"])
	if name == "" {
		name = "upload.bin"
	}
	expires := defaultExpires
	if s := info.MetaData["expires"]; s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			expires = d
		}
	}
	ct := info.MetaData["filetype"]
	if ct == "" {
		ct = "application/octet-stream"
	}

	mu.Lock()
	defer mu.Unlock()

	key, err := newKey()
	if err != nil {
		return tushandler.HTTPResponse{}, err
	}

	src := filepath.Join(tusDir, info.ID)
	dst := filepath.Join(dataDir, key)
	if err := os.Rename(src, dst); err != nil {
		return tushandler.HTTPResponse{}, fmt.Errorf("rename: %w", err)
	}
	_ = os.Remove(filepath.Join(tusDir, info.ID+".info"))

	now := time.Now().UTC()
	m := meta{
		Key: key, Name: name, ContentType: ct,
		Size: info.Size, CreatedAt: now, ExpiresAt: now.Add(expires),
	}
	if err := writeMeta(key, m); err != nil {
		_ = os.Remove(dst)
		return tushandler.HTTPResponse{}, err
	}

	// mapping pour /finalize/{id}
	mapPath := filepath.Join(tusDir, "done_"+info.ID+".json")
	mapData, _ := json.Marshal(map[string]any{
		"key":        key,
		"name":       name,
		"url":        fmt.Sprintf("%s/%s", baseURL, key),
		"url_named":  fmt.Sprintf("%s/%s/%s", baseURL, key, urlEncode(name)),
		"expires_at": m.ExpiresAt,
		"size":       info.Size,
	})
	if err := os.WriteFile(mapPath, mapData, 0o644); err != nil {
		log.Printf("finalize: write mapping: %v", err)
	}

	// Headers de la réponse 204 finale (utiles pour clients qui les lisent).
	return tushandler.HTTPResponse{
		Header: tushandler.HTTPHeader{
			"X-Tmpshare-Key": key,
			"X-Tmpshare-URL": fmt.Sprintf("%s/%s", baseURL, key),
		},
	}, nil
}

// /finalize/{tus-id} — récupère et consomme le mapping écrit par finalizeTus.
func finalize(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/finalize/")
	if id == "" || strings.ContainsAny(id, "/\\.") {
		http.Error(w, "id invalide", http.StatusBadRequest)
		return
	}
	mapPath := filepath.Join(tusDir, "done_"+id+".json")
	b, err := os.ReadFile(mapPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "pas encore prêt", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Remove(mapPath)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

// ----- store commun (PUT et multipart) -----

func store(name, contentType string, body io.Reader, expires time.Duration) (*uploadResponse, error) {
	mu.Lock()
	defer mu.Unlock()

	key, err := newKey()
	if err != nil {
		return nil, err
	}
	dataPath := filepath.Join(dataDir, key)
	out, err := os.Create(dataPath)
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(out, body)
	if err != nil {
		out.Close()
		os.Remove(dataPath)
		return nil, err
	}
	if err := out.Close(); err != nil {
		os.Remove(dataPath)
		return nil, err
	}

	now := time.Now().UTC()
	m := meta{
		Key: key, Name: name, ContentType: contentType,
		Size: n, CreatedAt: now, ExpiresAt: now.Add(expires),
	}
	if err := writeMeta(key, m); err != nil {
		os.Remove(dataPath)
		return nil, err
	}
	return &uploadResponse{
		Success:   true,
		Key:       key,
		Name:      name,
		Size:      n,
		URL:       fmt.Sprintf("%s/%s", baseURL, key),
		URLNamed:  fmt.Sprintf("%s/%s/%s", baseURL, key, urlEncode(name)),
		ExpiresAt: m.ExpiresAt,
	}, nil
}

// ----- download -----

func download(w http.ResponseWriter, r *http.Request, key string) {
	m, err := readMeta(key)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if time.Now().After(m.ExpiresAt) {
		_ = purge(key)
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// /{key} → redirect 302 vers /{key}/{nom} pour que wget/curl simples
	// reconstruisent le bon nom à partir du dernier segment de l'URL.
	expected := "/" + m.Key
	if r.URL.Path == expected || r.URL.Path == expected+"/" {
		http.Redirect(w, r, expected+"/"+urlEncode(m.Name), http.StatusFound)
		return
	}

	dataPath := filepath.Join(dataDir, m.Key)
	f, err := os.Open(dataPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", m.ContentType)
	// inline => le navigateur affiche si possible (image/vidéo/pdf), sinon télécharge.
	// curl -OJ et wget --content-disposition utilisent le filename pour nommer.
	w.Header().Set("Content-Disposition", contentDisposition("inline", m.Name))
	http.ServeContent(w, r, m.Name, st.ModTime(), f)
}

// contentDisposition encode le filename selon RFC 5987 (UTF-8) + variante ASCII.
func contentDisposition(disp, name string) string {
	ascii := strings.Map(func(r rune) rune {
		if r < 0x20 || r == '"' || r == '\\' || r > 0x7e {
			return '_'
		}
		return r
	}, name)
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disp, ascii, urlEncode(name))
}

func urlEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9',
			c == '-' || c == '_' || c == '.' || c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

// ----- list / delete -----

func list(w http.ResponseWriter, _ *http.Request) {
	mu.Lock()
	defer mu.Unlock()
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := []map[string]any{}
	now := time.Now()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".meta.json")
		m, err := readMetaUnlocked(key)
		if err != nil {
			continue
		}
		if now.After(m.ExpiresAt) {
			continue
		}
		out = append(out, map[string]any{
			"key":        m.Key,
			"name":       m.Name,
			"size":       m.Size,
			"created_at": m.CreatedAt,
			"expires_at": m.ExpiresAt,
			"url":        fmt.Sprintf("%s/%s", baseURL, m.Key),
			"url_named":  fmt.Sprintf("%s/%s/%s", baseURL, m.Key, urlEncode(m.Name)),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"count": len(out), "files": out})
}

func apiDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/")
	if !validKey(key) {
		http.NotFound(w, r)
		return
	}
	if _, err := readMeta(key); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := purge(key); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "key": key})
}

// ----- janitor -----

func janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		sweep()
		<-t.C
	}
}

func sweep() {
	mu.Lock()
	defer mu.Unlock()
	now := time.Now()

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		log.Printf("janitor: %v", err)
		return
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		key := strings.TrimSuffix(e.Name(), ".meta.json")
		m, err := readMetaUnlocked(key)
		if err != nil {
			continue
		}
		if now.After(m.ExpiresAt) {
			if err := purgeUnlocked(key); err != nil {
				log.Printf("janitor purge %s: %v", key, err)
			} else {
				log.Printf("janitor: %s (%s) expiré, supprimé", key, m.Name)
			}
		}
	}

	// Nettoie aussi les mappings done_*.json non consommés > 1h
	tusEntries, err := os.ReadDir(tusDir)
	if err != nil {
		return
	}
	for _, e := range tusEntries {
		if !strings.HasPrefix(e.Name(), "done_") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(tusDir, e.Name()))
		}
	}
}

func purge(key string) error {
	mu.Lock()
	defer mu.Unlock()
	return purgeUnlocked(key)
}

func purgeUnlocked(key string) error {
	if !validKey(key) {
		return errors.New("clé invalide")
	}
	dataErr := os.Remove(filepath.Join(dataDir, key))
	metaErr := os.Remove(filepath.Join(dataDir, key+".meta.json"))
	if dataErr != nil && !errors.Is(dataErr, os.ErrNotExist) {
		return dataErr
	}
	if metaErr != nil && !errors.Is(metaErr, os.ErrNotExist) {
		return metaErr
	}
	return nil
}

// ----- meta I/O -----

func writeMeta(key string, m meta) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, key+".meta.json"), b, 0o644)
}

func readMeta(key string) (meta, error) {
	mu.Lock()
	defer mu.Unlock()
	return readMetaUnlocked(key)
}

func readMetaUnlocked(key string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(dataDir, key+".meta.json"))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(b, &m)
	return m, err
}

// ----- utils -----

func parseExpires(s string) (time.Duration, error) {
	if s == "" {
		return defaultExpires, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, errors.New("doit être > 0")
	}
	return d, nil
}

func newKey() (string, error) {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// 6 octets → 10 chars base32 (sans padding) en minuscules.
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func validKey(s string) bool {
	if len(s) < 6 || len(s) > 32 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

// sanitizeName porte werkzeug.utils.secure_filename : NFKD + strip non-ASCII,
// remplace les séparateurs de chemin par des espaces, condense les espaces en "_",
// ne garde que [A-Za-z0-9_.-], strip "._" en bord, préfixe les noms de devices Windows.
// Élimine ainsi tout caractère shell-actif ($ ` ; | & < > etc.) et tout path traversal.
func sanitizeName(s string) string {
	s = norm.NFKD.String(s)

	var ascii strings.Builder
	for _, r := range s {
		if r < 0x80 {
			ascii.WriteRune(r)
		}
	}
	s = ascii.String()

	s = strings.ReplaceAll(s, "/", " ")
	s = strings.ReplaceAll(s, "\\", " ")
	s = strings.Join(strings.Fields(s), "_")

	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		}
	}
	s = strings.Trim(b.String(), "._")
	if s == "" {
		return ""
	}
	base, _, _ := strings.Cut(s, ".")
	if windowsDeviceFiles[strings.ToUpper(base)] {
		s = "_" + s
	}
	return s
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

