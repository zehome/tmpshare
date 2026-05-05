import { h, render } from 'preact';
import { useState, useRef, useEffect } from 'preact/hooks';
import htm from 'htm';
import * as tus from 'tus-js-client';

const html = htm.bind(h);

const TOKEN_KEY = 'tmpshare.token';
const CHUNK = 64 * 1024 * 1024;
const TOAST_MS = 1600;

const STATUS_MARKER = { uploading: '▸', done: '✓', error: '×' };

function fmtSize(n) {
  if (n == null) return '—';
  if (n === 0) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(n < 10 && i > 0 ? 1 : 0) + ' ' + u[i];
}
function fmtSpeed(b) { return (b > 0 ? fmtSize(b) : '0 B') + '/s'; }
function fmtETA(secs) {
  if (!isFinite(secs) || secs < 0) return '—';
  const s = Math.floor(secs);
  if (s < 60) return s + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return m + 'm ' + String(s % 60).padStart(2, '0') + 's';
  const hr = Math.floor(m / 60);
  return hr + 'h ' + String(m % 60).padStart(2, '0') + 'm';
}
function fmtTime(d) {
  return new Date(d).toLocaleString(undefined, { dateStyle: 'short', timeStyle: 'short' });
}

const authHeaders = (token) => ({ 'X-Auth-Token': token });

function App() {
  const [token, setToken]         = useState(() => localStorage.getItem(TOKEN_KEY) || '');
  const [draftToken, setDraft]    = useState('');
  const [expires, setExpires]     = useState('');
  const [uploads, setUploads]     = useState([]);
  const [authMode, setAuthMode]   = useState('loading');
  const [authState, setAuthState] = useState('idle');
  const [toast, setToast]         = useState('');
  const [files, setFiles]         = useState([]);
  const [filesLoading, setFilesLoading] = useState(false);
  const [filesErr, setFilesErr]   = useState('');
  const [dragOver, setDragOver]   = useState(false);
  const [version, setVersion]     = useState('');
  const fileInputRef = useRef(null);
  const toastTimer   = useRef(null);

  useEffect(() => {
    fetch('/auth', { headers: token ? authHeaders(token) : {} })
      .then(r => {
        if (r.status === 401) {
          if (token) { localStorage.removeItem(TOKEN_KEY); setToken(''); }
          setAuthMode('no-token');
          return;
        }
        if (!r.ok) throw new Error('HTTP ' + r.status);
        setAuthMode('authed');
      })
      .catch(() => setAuthMode('no-token'));
  }, []);

  const loadFiles = async () => {
    setFilesLoading(true);
    setFilesErr('');
    try {
      const r = await fetch('/list', { headers: authHeaders(token) });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      const data = await r.json();
      const list = (data.files || []).slice().sort(
        (a, b) => new Date(b.created_at) - new Date(a.created_at),
      );
      setFiles(list);
    } catch (e) {
      setFilesErr(String(e.message || e));
    } finally {
      setFilesLoading(false);
    }
  };

  useEffect(() => {
    if (authMode === 'authed') loadFiles();
  }, [authMode]);

  useEffect(() => {
    fetch('/version')
      .then(r => r.ok ? r.json() : null)
      .then(d => d && setVersion(d.version))
      .catch(() => {});
  }, []);

  const showToast = (msg) => {
    setToast(msg);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(''), TOAST_MS);
  };

  const deleteFile = async (key, name) => {
    if (!confirm('Supprimer « ' + name + ' » ?')) return;
    try {
      const r = await fetch('/api/' + encodeURIComponent(key), {
        method: 'DELETE',
        headers: authHeaders(token),
      });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      setFiles(fs => fs.filter(f => f.key !== key));
      showToast('Supprimé · ' + name);
    } catch (_) {
      showToast('Erreur suppression');
    }
  };

  const saveToken = async () => {
    const t = draftToken.trim();
    if (!t) return;
    setAuthState('checking');
    try {
      const r = await fetch('/auth', { headers: authHeaders(t) });
      if (r.status === 401) { setAuthState('bad'); return; }
      if (!r.ok) throw new Error('HTTP ' + r.status);
      localStorage.setItem(TOKEN_KEY, t);
      setToken(t);
      setAuthMode('authed');
      setDraft(''); setAuthState('idle');
    } catch (_) { setAuthState('bad'); }
  };

  const clearToken = () => {
    if (uploads.some(u => u.status === 'uploading') &&
        !confirm('Des uploads sont en cours. Déconnexion ?')) return;
    localStorage.removeItem(TOKEN_KEY);
    setToken(''); setUploads([]); setAuthMode('no-token');
  };

  const startUpload = (file) => {
    const id = Math.random().toString(36).slice(2, 10);
    const meta = { filename: file.name, filetype: file.type || 'application/octet-stream' };
    if (expires.trim()) meta.expires = expires.trim();

    const upd = (patch) => setUploads(u => u.map(x => x.id === id ? { ...x, ...patch } : x));
    const startedAt = Date.now();

    setUploads(u => [{
      id, name: file.name, size: file.size,
      progress: 0, sent: 0, speed: 0, eta: Infinity,
      status: 'uploading', url: null, urlNamed: null, key: null,
      expiresAt: null, err: null,
    }, ...u]);

    const upload = new tus.Upload(file, {
      endpoint: '/files/',
      retryDelays: [0, 1000, 3000, 5000, 10000, 20000, 30000],
      chunkSize: CHUNK,
      metadata: meta,
      headers: authHeaders(token),
      onError: (err) => upd({ status: 'error', err: String(err.message || err) }),
      onProgress: (sent, total) => {
        const elapsed = (Date.now() - startedAt) / 1000;
        const speed = elapsed > 0 ? sent / elapsed : 0;
        const eta = speed > 0 ? (total - sent) / speed : Infinity;
        upd({ progress: total ? sent / total : 0, sent, speed, eta });
      },
      onSuccess: async () => {
        const tusId = upload.url.split('/').filter(Boolean).pop();
        try {
          const r = await fetch('/finalize/' + tusId, { headers: authHeaders(token) });
          if (!r.ok) throw new Error('finalize ' + r.status);
          const data = await r.json();
          upd({
            status: 'done', progress: 1,
            url: data.url, urlNamed: data.url_named,
            key: data.key, expiresAt: data.expires_at,
          });
          loadFiles();
        } catch (e) {
          upd({ status: 'error', err: 'finalize: ' + e.message });
        }
      },
    });
    upload.start();
  };

  const onPick = (ev) => { [...ev.target.files].forEach(startUpload); ev.target.value = ''; };
  const onDrop = (ev) => {
    ev.preventDefault();
    setDragOver(false);
    [...ev.dataTransfer.files].forEach(startUpload);
  };

  const copy = async (text, label) => {
    try { await navigator.clipboard.writeText(text); showToast('Copié · ' + label); }
    catch (_) { showToast('Erreur copie'); }
  };

  if (authMode === 'loading') return null;
  if (authMode === 'no-token') return html`<${AuthScreen}
      draft=${draftToken}
      onChangeDraft=${v => { setDraft(v); if (authState === 'bad') setAuthState('idle'); }}
      onSubmit=${saveToken}
      state=${authState} />`;

  const active = uploads.filter(u => u.status === 'uploading').length;
  const done   = uploads.filter(u => u.status === 'done').length;
  const failed = uploads.filter(u => u.status === 'error').length;

  return html`
    <div class="topbar">
      <div class="left">
        <span><span class="dot"></span>session active</span>
      </div>
      <div class="right">
        ${authMode === 'authed' && html`<button class="btn-danger" onClick=${clearToken}>déconnexion</button>`}
      </div>
    </div>

    <div class="hero">
      <h1>tmp<span class="slash">/</span>share</h1>
    </div>

    <div class="card tick">
      <div class=${'drop' + (dragOver ? ' over' : '')}
           onClick=${() => fileInputRef.current.click()}
           onDragOver=${e => { e.preventDefault(); setDragOver(true); }}
           onDragLeave=${() => setDragOver(false)}
           onDrop=${onDrop}>
        <input type="file" multiple ref=${fileInputRef} onChange=${onPick} />
        <div class="glyph">[ + ]</div>
        <div class="main">Glissez un fichier ici, ou cliquez pour sélectionner</div>
        <div class="sub">tus protocol · resumable · multi-fichiers</div>
      </div>
      <div class="opts">
        <div class="label">expiration</div>
        <input type="text" class="input"
               placeholder="24h, 7d, 168h…"
               value=${expires}
               onInput=${e => setExpires(e.target.value)} />
        <div class="help">vide = défaut serveur (<code>168h</code> · 1 semaine)</div>
      </div>
    </div>

    <div class="section-head">
      <h2>Uploads</h2>
      <span class="count">
        ${active > 0 ? active + ' actif · ' : ''}${done} terminé${done > 1 ? 's' : ''}${failed > 0 ? ' · ' + failed + ' erreur' + (failed > 1 ? 's' : '') : ''}
      </span>
    </div>

    ${uploads.length === 0
      ? html`<div class="empty">aucun upload pour cette session</div>`
      : uploads.map(u => html`<${UploadItem} key=${u.id} u=${u} copy=${copy} />`)}

    <div class="section-head">
      <h2>Fichiers</h2>
      <span class="count">
        ${filesLoading ? 'chargement…' : files.length + ' fichier' + (files.length > 1 ? 's' : '')}
        ${' '}
        <button class="btn-ghost" onClick=${loadFiles} disabled=${filesLoading}
                style="margin-left:10px; padding:4px 10px; font-size:10px;">↻ rafraîchir</button>
      </span>
    </div>

    ${filesErr && html`<div class="err-text">${filesErr}</div>`}

    ${!filesLoading && !filesErr && files.length === 0
      ? html`<div class="empty">aucun fichier sur le serveur</div>`
      : files.map(f => html`<${ServerFileItem} key=${f.key} f=${f} copy=${copy}
                                                onDelete=${() => deleteFile(f.key, f.name)} />`)}

    ${toast && html`<div class="toast">${toast}</div>`}

    <div class="footer">build ${version || '—'}</div>
  `;
}

function AuthScreen({ draft, onChangeDraft, onSubmit, state }) {
  const checking = state === 'checking';
  return html`
    <div class="auth-wrap">
      <div class="hero">
        <h1>tmp<span class="slash">/</span>share<span class="cur">█</span></h1>
        <div class="tag">partage temporaire de fichiers · resumable upload</div>
      </div>
      <div class="auth-card">
        <div class="auth-status"><span class="sq"></span>auth required</div>
        <div class="label">Jeton d'accès</div>
        <div class="row">
          <input type="password" class="input" autoFocus
            value=${draft}
            onInput=${e => onChangeDraft(e.target.value)}
            onKeyDown=${e => e.key === 'Enter' && !checking && onSubmit()}
            placeholder="UPLOAD_TOKEN"
            disabled=${checking}
            autocomplete="current-password" />
          <button class="btn" onClick=${onSubmit} disabled=${checking || !draft.trim()}>
            ${checking ? html`<span class="loader"></span> Vérification` : '→ Connexion'}
          </button>
        </div>
        ${state === 'bad' && html`<div class="err-text">Jeton invalide · refusé par le serveur</div>`}
        <div class="help" style="margin-top:18px">
          Le jeton est conservé en <code>localStorage</code> uniquement après validation côté serveur.<br/>
          Les téléchargements restent publics via l'URL générée — partagez-la uniquement aux destinataires.
        </div>
      </div>
    </div>
  `;
}

function UrlRow({ label, url, copyLabel, copy }) {
  return html`
    <div class="url-row">
      <span class="ulabel">${label}</span>
      <div class="uval">${url}</div>
      <button class="btn-ghost" onClick=${() => copy(url, copyLabel)}>copier</button>
    </div>
  `;
}

function ServerFileItem({ f, copy, onDelete }) {
  const named = f.url_named || f.url;
  return html`
    <div class="item">
      <div class="item-head">
        <div class="name"><span class="marker" style="color: var(--text-2);">●</span>${f.name}</div>
        <div style="display:flex; gap:12px; align-items:center; flex-shrink:0;">
          <span class="size">${fmtSize(f.size)}</span>
          <button class="btn-danger" onClick=${onDelete}>supprimer</button>
        </div>
      </div>
      <div class="urls">
        <${UrlRow} label="court" url=${f.url} copyLabel="URL courte" copy=${copy} />
        <${UrlRow} label="+nom"  url=${named} copyLabel="URL avec nom" copy=${copy} />
      </div>
      <div class="done-meta stats">
        <div class="stat"><span class="k">créé</span><span class="v">${fmtTime(f.created_at)}</span></div>
        <div class="stat"><span class="k">expire</span><span class="v ok">${fmtTime(f.expires_at)}</span></div>
        <div class="stat"><span class="k">clé</span><span class="v">${f.key}</span></div>
      </div>
    </div>
  `;
}

function UploadItem({ u, copy }) {
  const marker = STATUS_MARKER[u.status] || '··';
  const pct = (u.progress * 100).toFixed(1);
  const named = u.urlNamed || u.url;

  return html`
    <div class="item ${u.status}">
      <div class="item-head">
        <div class="name"><span class="marker">${marker}</span>${u.name}</div>
        <div class="size">${fmtSize(u.size)}</div>
      </div>

      ${u.status === 'uploading' && html`
        <div class="progress"><div class="fill" style=${{ width: pct + '%' }}></div></div>
        <div class="ticks"><span>0</span><span>25</span><span>50</span><span>75</span><span>100</span></div>
        <div class="stats">
          <div class="stat"><span class="k">progrès</span><span class="v accent">${pct}%</span></div>
          <div class="stat"><span class="k">débit</span><span class="v">${fmtSpeed(u.speed)}</span></div>
          <div class="stat"><span class="k">restant</span><span class="v">${fmtETA(u.eta)}</span></div>
          <div class="stat"><span class="k">envoyé</span><span class="v">${fmtSize(u.sent)}</span></div>
        </div>
      `}

      ${u.status === 'done' && html`
        <div class="urls">
          <${UrlRow} label="court" url=${u.url} copyLabel="URL courte" copy=${copy} />
          <${UrlRow} label="+nom"  url=${named} copyLabel="URL avec nom" copy=${copy} />
        </div>
        <div class="done-meta stats">
          <div class="stat"><span class="k">expire</span><span class="v ok">${fmtTime(u.expiresAt)}</span></div>
          <div class="stat"><span class="k">clé</span><span class="v">${u.key}</span></div>
        </div>
      `}

      ${u.status === 'error' && html`<div class="err-text">${u.err}</div>`}
    </div>
  `;
}

render(html`<${App} />`, document.getElementById('root'));
