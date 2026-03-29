package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"html"
	"html/template"
	"io"
	"log"
	"math/big"
	"mime"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/gabriel-vasile/mimetype"
	"golang.org/x/crypto/bcrypt"
)

// ---- Config ----------------------------------------------------------------

type Config struct {
	Webroot        string `toml:"webroot"`
	Lport          string `toml:"lport"`
	Vhost          string `toml:"vhost"`
	Filelen        int    `toml:"filelen"`
	Folder         string `toml:"folder"`
	Bgfolder       string `toml:"bgfolder"`
	MaxUploadMB    int64  `toml:"max_upload_mb"`
	TTLHours       int    `toml:"ttl_hours"`
	RateLimitPerMin int   `toml:"rate_limit_per_min"`
}

// ---- Meta sidecar ----------------------------------------------------------

type FileMeta struct {
	PasswordHash string `json:"password_hash"`
}

// ---- Rate limiter ----------------------------------------------------------

type ipRecord struct {
	mu        sync.Mutex
	timestamps []time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	records map[string]*ipRecord
	limit   int
}

func NewRateLimiter(limit int) *RateLimiter {
	return &RateLimiter{records: make(map[string]*ipRecord), limit: limit}
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	rec, ok := rl.records[ip]
	if !ok {
		rec = &ipRecord{}
		rl.records[ip] = rec
	}
	rl.mu.Unlock()

	rec.mu.Lock()
	defer rec.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-time.Minute)
	fresh := rec.timestamps[:0]
	for _, t := range rec.timestamps {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}
	rec.timestamps = fresh
	if len(rec.timestamps) >= rl.limit {
		return false
	}
	rec.timestamps = append(rec.timestamps, now)
	return true
}

// Periodically purge empty records to prevent unbounded map growth.
func (rl *RateLimiter) cleanup(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rl.mu.Lock()
			for ip, rec := range rl.records {
				rec.mu.Lock()
				if len(rec.timestamps) == 0 {
					delete(rl.records, ip)
				}
				rec.mu.Unlock()
			}
			rl.mu.Unlock()
		}
	}
}

// ---- Globals ---------------------------------------------------------------

var (
	conf        Config
	activeFiles sync.Map // filename → struct{}{} for active downloads
	rateLimiter *RateLimiter
)

// ---- ID generation ---------------------------------------------------------

const idChars = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKMNPQRSTUVWXYZ23456789"

func genID(length int) (string, error) {
	b := make([]byte, length)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(idChars))))
		if err != nil {
			return "", err
		}
		b[i] = idChars[n.Int64()]
	}
	return string(b), nil
}

// ---- Shredding -------------------------------------------------------------

const chunkSize = 32 * 1024

func shredFile(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	size := fi.Size()

	buf := make([]byte, chunkSize)
	// 7 random passes
	for pass := 0; pass < 7; pass++ {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			f.Close()
			return err
		}
		remaining := size
		for remaining > 0 {
			n := int64(chunkSize)
			if remaining < n {
				n = remaining
			}
			if _, err := rand.Read(buf[:n]); err != nil {
				f.Close()
				return err
			}
			if _, err := f.Write(buf[:n]); err != nil {
				f.Close()
				return err
			}
			remaining -= n
		}
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	// Zero fill pass
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	for i := range buf {
		buf[i] = 0
	}
	remaining := size
	for remaining > 0 {
		n := int64(chunkSize)
		if remaining < n {
			n = remaining
		}
		if _, err := f.Write(buf[:n]); err != nil {
			f.Close()
			return err
		}
		remaining -= n
	}
	f.Sync()
	f.Close()
	return os.Remove(path)
}

// ---- Cleanup goroutine -----------------------------------------------------

func cullLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cullOnce()
		}
	}
}

func cullOnce() {
	entries, err := os.ReadDir(conf.Folder)
	if err != nil {
		log.Printf("cull: readdir: %v", err)
		return
	}
	ttl := time.Duration(conf.TTLHours) * time.Hour
	now := time.Now()
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".meta") {
			continue
		}
		if _, active := activeFiles.Load(name); active {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(fi.ModTime()) > ttl {
			path := filepath.Join(conf.Folder, name)
			log.Printf("cull: shredding %s", name)
			if err := shredFile(path); err != nil {
				log.Printf("cull: shred %s: %v", name, err)
			}
			metaPath := path + ".meta"
			if _, err := os.Stat(metaPath); err == nil {
				os.Remove(metaPath)
			}
		}
	}
}

// ---- Path safety -----------------------------------------------------------

func safePath(baseDir, name string) (string, bool) {
	name = filepath.Base(name)
	joined := filepath.Join(baseDir, name)
	if !strings.HasPrefix(joined, filepath.Clean(baseDir)+string(os.PathSeparator)) {
		return "", false
	}
	return joined, true
}

// ---- MIME inline allowlist -------------------------------------------------

func isInlineMIME(mtype string) bool {
	major := strings.SplitN(mtype, "/", 2)[0]
	switch major {
	case "image", "audio", "video":
		return true
	}
	switch mtype {
	case "application/pdf", "text/plain":
		return true
	}
	return false
}

// ---- Meta helpers ----------------------------------------------------------

func readMeta(metaPath string) (*FileMeta, error) {
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}
	var m FileMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func writeMeta(metaPath string, m *FileMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath, data, 0644)
}

// ---- Templates -------------------------------------------------------------

const passwordPageTmpl = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HARDFILES — PASSWORD REQUIRED</title>
<link rel="icon" href="/static/fist.ico">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=VT323&display=swap">
<style>
:root {
  --bg: #000;
  --overlay: rgba(0,0,0,0.45);
  --red: #FF0000;
  --cyan: #00FFFF;
  --green: #00FF00;
  --yellow: #FFFF00;
  --text: #FFF;
  --text-dim: #888;
  --font: 'VT323', monospace;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%;
  background: var(--bg);
  color: var(--text);
  font-family: var(--font);
  font-size: 20px;
}
body {
  background-image: url('{{.BgURL}}');
  background-size: cover;
  background-position: center;
  background-attachment: fixed;
}
body::before {
  content: '';
  position: fixed;
  inset: 0;
  background: var(--overlay);
  pointer-events: none;
  z-index: 0;
}
body::after {
  content: '';
  position: fixed;
  inset: 0;
  background: repeating-linear-gradient(
    to bottom,
    transparent 0px,
    transparent 2px,
    rgba(0,0,0,0.4) 2px,
    rgba(0,0,0,0.4) 3px
  );
  pointer-events: none;
  z-index: 1;
}
.container {
  position: relative;
  z-index: 2;
  min-height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 2rem;
}
.logo { width: 30%; max-width: 300px; margin-bottom: 2rem; }
.filename { color: var(--text-dim); margin-bottom: 2rem; font-size: 1.2rem; }
form { display: flex; flex-direction: column; gap: 1rem; width: 320px; max-width: 90vw; }
input[type=password] {
  background: #000;
  border: 2px dashed var(--red);
  color: var(--text);
  font-family: var(--font);
  font-size: 1.4rem;
  padding: 0.6rem 1rem;
  outline: none;
  width: 100%;
}
input[type=password]:focus { border-style: solid; }
button[type=submit] {
  background: var(--red);
  color: #000;
  border: none;
  font-family: var(--font);
  font-size: 1.4rem;
  padding: 0.6rem 1rem;
  cursor: pointer;
  text-transform: uppercase;
  min-height: 44px;
}
button[type=submit]:hover { background: #cc0000; }
.error { color: var(--red); font-size: 1.2rem; min-height: 1.5rem; }
</style>
</head>
<body>
<div class="container">
  <img class="logo" src="/static/header.png" alt="HARDFILES">
  <div class="filename">{{.Filename}}</div>
  <form method="POST" action="/f/{{.FileID}}">
    <label for="pw" style="color:var(--text-dim)">PASSWORD REQUIRED</label>
    <input type="password" id="pw" name="password" autofocus placeholder="enter password" aria-label="File password">
    <button type="submit">UNLOCK</button>
    <div class="error">{{.Error}}</div>
  </form>
</div>
</body>
</html>`

var passwordTpl = template.Must(template.New("pw").Parse(passwordPageTmpl))

// ---- Index handler ---------------------------------------------------------

const indexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>HARDFILES</title>
<link rel="icon" href="/static/fist.ico">
<link rel="stylesheet" href="https://fonts.googleapis.com/css2?family=VT323&display=swap">
<style>
:root {
  --bg: #000000;
  --overlay: rgba(0,0,0,0.45);
  --red: #FF0000;
  --cyan: #00FFFF;
  --green: #00FF00;
  --yellow: #FFFF00;
  --text: #FFFFFF;
  --text-dim: #888888;
  --font: 'VT323', monospace;
}
* { box-sizing: border-box; margin: 0; padding: 0; }
html, body {
  height: 100%;
  overflow: hidden;
  background: var(--bg);
  color: var(--text);
  font-family: var(--font);
  font-size: 20px;
}
#bg {
  position: fixed;
  inset: 0;
  background-image: url('{{.BgURL}}');
  background-size: cover;
  background-position: center;
  opacity: 0;
  transition: opacity 0.5s ease;
  z-index: 0;
}
#bg.loaded { opacity: 1; }
body::before {
  content: '';
  position: fixed;
  inset: 0;
  background: var(--overlay);
  pointer-events: none;
  z-index: 1;
}
body::after {
  content: '';
  position: fixed;
  inset: 0;
  background: repeating-linear-gradient(
    to bottom,
    transparent 0px,
    transparent 2px,
    rgba(0,0,0,0.12) 2px,
    rgba(0,0,0,0.12) 3px
  );
  pointer-events: none;
  z-index: 2;
}
.wrap {
  position: relative;
  z-index: 3;
  height: 100vh;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 1rem;
  gap: 0.5rem;
}
/* ---- Logo glitch ---- */
.logo-wrap {
  position: relative;
  width: 50%;
  max-width: 600px;
  margin-bottom: 0.3rem;
}
@media (max-width: 768px) { .logo-wrap { width: 90%; } }
.logo-wrap img {
  width: 100%;
  display: block;
  position: relative;
  z-index: 1;
}
.logo-wrap::before,
.logo-wrap::after {
  content: '';
  position: absolute;
  inset: 0;
  background-image: url('/static/header.png');
  background-size: 100% 100%;
  background-repeat: no-repeat;
  z-index: 2;
}
.logo-wrap::before {
  animation: glitch-before 2s infinite steps(1);
}
.logo-wrap::after {
  animation: glitch-after 2s infinite steps(1);
  animation-delay: 0.1s;
}
@keyframes glitch-before {
  0%,4%   { opacity: 0; }
  5%      { opacity: 0.8; transform: translate(-5px, 2px); filter: hue-rotate(90deg);
            clip-path: polygon(0 15%, 100% 15%, 100% 35%, 0 35%); }
  7%      { opacity: 0.8; transform: translate(5px, -2px); filter: hue-rotate(180deg);
            clip-path: polygon(0 55%, 100% 55%, 100% 75%, 0 75%); }
  8%,29%  { opacity: 0; }
  30%     { opacity: 0.7; transform: translate(-3px, -1px); filter: hue-rotate(270deg);
            clip-path: polygon(0 40%, 100% 40%, 100% 60%, 0 60%); }
  32%,59% { opacity: 0; }
  60%     { opacity: 0.9; transform: translate(6px, 1px); filter: hue-rotate(120deg);
            clip-path: polygon(0 10%, 100% 10%, 100% 25%, 0 25%); }
  61%     { opacity: 0.9; transform: translate(-4px, 3px); filter: hue-rotate(200deg);
            clip-path: polygon(0 70%, 100% 70%, 100% 90%, 0 90%); }
  63%,84% { opacity: 0; }
  85%     { opacity: 0.7; transform: translate(3px, -2px); filter: hue-rotate(45deg);
            clip-path: polygon(0 30%, 100% 30%, 100% 50%, 0 50%); }
  87%,100%{ opacity: 0; }
}
@keyframes glitch-after {
  0%,14%  { opacity: 0; }
  15%     { opacity: 0.7; transform: translate(4px, 2px); filter: hue-rotate(300deg);
            clip-path: polygon(0 20%, 100% 20%, 100% 45%, 0 45%); }
  17%,44% { opacity: 0; }
  45%     { opacity: 0.8; transform: translate(-6px, -1px); filter: hue-rotate(60deg);
            clip-path: polygon(0 60%, 100% 60%, 100% 80%, 0 80%); }
  47%     { opacity: 0.6; transform: translate(3px, 3px); filter: hue-rotate(150deg);
            clip-path: polygon(0 5%, 100% 5%, 100% 20%, 0 20%); }
  49%,74% { opacity: 0; }
  75%     { opacity: 0.9; transform: translate(-5px, 2px); filter: hue-rotate(240deg);
            clip-path: polygon(0 45%, 100% 45%, 100% 65%, 0 65%); }
  77%,100%{ opacity: 0; }
}

/* ---- Tagline ---- */
.tagline {
  color: var(--red);
  font-size: 1.4rem;
  letter-spacing: 0.15em;
  margin-bottom: 0.5rem;
  text-align: center;
}

/* ---- Drop zone ---- */
.drop-zone {
  position: relative;
  width: 440px;
  max-width: 90vw;
  min-height: 150px;
  border: 2px dashed var(--red);
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  padding: 1.5rem;
  cursor: pointer;
  background: transparent;
  color: var(--text);
  font-family: var(--font);
  text-align: center;
  animation: pulse-opacity 2s ease-in-out infinite;
  transition: background 0.2s, border-style 0.1s;
}
@media (max-width: 768px) {
  .drop-zone { width: 90vw; min-height: 130px; }
}
@keyframes pulse-opacity {
  0%, 100% { opacity: 1; }
  50% { opacity: 0.55; }
}
.drop-zone:hover, .drop-zone.drag-over {
  animation: shake 0.3s ease;
  opacity: 1;
  border-style: solid;
  background: rgba(255,0,0,0.07);
}
@keyframes shake {
  0%, 100% { transform: translateX(0); }
  20% { transform: translateX(-4px); }
  40% { transform: translateX(4px); }
  60% { transform: translateX(-3px); }
  80% { transform: translateX(3px); }
}
.drop-zone.uploading {
  animation: pulse-opacity 0.5s ease-in-out infinite;
  border-style: dashed;
  border-color: var(--red);
}
.drop-zone.success {
  animation: none;
  opacity: 1;
  border-style: solid;
  border-color: var(--green);
}
.drop-zone.error-state {
  animation: none;
  opacity: 1;
  border-style: solid;
  border-color: var(--red);
}
.drop-main {
  font-size: 2rem;
  letter-spacing: 0.1em;
  margin-bottom: 0.5rem;
}
.drop-sub {
  font-size: 1.1rem;
  color: var(--text-dim);
}
.drop-url {
  font-size: 1.6rem;
  color: var(--red);
  word-break: break-all;
  cursor: pointer;
  margin-bottom: 0.5rem;
}
.drop-url:hover { text-decoration: underline; }
.drop-another {
  font-size: 1rem;
  color: var(--text-dim);
  cursor: pointer;
  margin-top: 0.5rem;
  text-decoration: underline;
  background: none;
  border: none;
  font-family: var(--font);
}
.drop-another:hover { color: var(--text); }
#file-input { display: none; }
#pw-row {
  display: flex;
  align-items: center;
  gap: 0.75rem;
  margin-top: 0.3rem;
  font-size: 1.2rem;
  color: var(--text-dim);
}
#pw-row input[type=checkbox] { width: 18px; height: 18px; cursor: pointer; }
#pw-input-wrap { margin-top: 0.3rem; }
#pw-input {
  background: #000;
  border: 2px dashed var(--red);
  color: var(--text);
  font-family: var(--font);
  font-size: 1.3rem;
  padding: 0.5rem 0.8rem;
  width: 440px;
  max-width: 90vw;
  outline: none;
}
#pw-input:focus { border-style: solid; }

/* ---- Curl example ---- */
.curl-box {
  margin-top: 0.5rem;
  max-width: 90vw;
  border-left: 3px solid var(--green);
  padding: 0.3rem 0.8rem;
  background: rgba(0,255,0,0.04);
}
.curl-box code {
  color: var(--green);
  font-family: var(--font);
  font-size: 0.85rem;
  text-shadow: 0 0 8px rgba(0,255,0,0.5);
  white-space: nowrap;
  display: block;
}

/* ---- Warning ---- */
.warning {
  margin-top: 0.5rem;
  color: var(--yellow);
  font-size: 1.3rem;
  letter-spacing: 0.08em;
  text-align: center;
  animation: flicker 4s infinite;
}
@keyframes flicker {
  0%,95%,100% { opacity: 1; }
  96% { opacity: 0.3; }
  97% { opacity: 1; }
  98% { opacity: 0.5; }
  99% { opacity: 1; }
}

/* ---- Live region ---- */
#live-region { position: absolute; left: -9999px; top: 0; }
</style>
</head>
<body>
<div id="bg"></div>
<div class="wrap">
  <div class="logo-wrap">
    <img src="/static/header.png" alt="HARDFILES">
  </div>
  <div class="tagline">ephemeral &bull; volatile &bull; gone in 24h</div>

  <button class="drop-zone" id="drop-zone" aria-label="Upload file — click, drag, or paste">
    <input type="file" id="file-input" aria-hidden="true" tabindex="-1">
    <div class="drop-main" id="drop-main">DROP FILE HERE</div>
    <div class="drop-sub" id="drop-sub">click to browse &bull; ctrl+v to paste &bull; drag &amp; drop</div>
  </button>

  <div aria-live="polite" id="live-region"></div>

  <div id="pw-row">
    <input type="checkbox" id="pw-check" aria-label="Password protect this file">
    <label for="pw-check">password protect</label>
  </div>
  <div id="pw-input-wrap" style="display:none">
    <label for="pw-input" style="display:none">File password</label>
    <input type="password" id="pw-input" placeholder="set password" autocomplete="new-password" aria-label="File password">
  </div>

  <div class="curl-box">
    <code>curl -F file=@example.png https://hardfiles.org/</code>
  </div>

  <div class="warning">ALL UPLOADS ARE SHREDDED AFTER 24 HOURS</div>
</div>

<script>
(function() {
  // Background fade-in
  var bgEl = document.getElementById('bg');
  if (bgEl.style.backgroundImage || bgEl.getAttribute('style')) {
    var img = new Image();
    var url = bgEl.style.backgroundImage.replace(/url\(["']?([^"')]+)["']?\)/, '$1');
    img.onload = function() { bgEl.classList.add('loaded'); };
    img.src = url;
  } else {
    // No BG URL set — still show the element (black)
    bgEl.classList.add('loaded');
  }

  var dropZone = document.getElementById('drop-zone');
  var fileInput = document.getElementById('file-input');
  var dropMain = document.getElementById('drop-main');
  var dropSub = document.getElementById('drop-sub');
  var liveRegion = document.getElementById('live-region');
  var pwCheck = document.getElementById('pw-check');
  var pwInputWrap = document.getElementById('pw-input-wrap');
  var pwInput = document.getElementById('pw-input');

  // Password toggle
  pwCheck.addEventListener('change', function() {
    pwInputWrap.style.display = pwCheck.checked ? 'block' : 'none';
    if (pwCheck.checked) pwInput.focus();
  });

  // Click to open file picker (but not when clicking pw area)
  dropZone.addEventListener('click', function(e) {
    if (e.target === pwCheck || e.target === pwInput || e.target.closest('#pw-row') || e.target.closest('#pw-input-wrap')) return;
    if (dropZone.classList.contains('success')) return;
    fileInput.click();
  });

  fileInput.addEventListener('change', function() {
    if (fileInput.files && fileInput.files[0]) {
      uploadFile(fileInput.files[0]);
    }
  });

  // Drag and drop
  dropZone.addEventListener('dragover', function(e) {
    e.preventDefault();
    dropZone.classList.add('drag-over');
  });
  dropZone.addEventListener('dragleave', function() {
    dropZone.classList.remove('drag-over');
  });
  dropZone.addEventListener('drop', function(e) {
    e.preventDefault();
    dropZone.classList.remove('drag-over');
    var files = e.dataTransfer.files;
    if (files && files[0]) uploadFile(files[0]);
  });

  // Paste
  document.addEventListener('paste', function(e) {
    var items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    for (var i = 0; i < items.length; i++) {
      var item = items[i];
      if (item.kind === 'file') {
        var file = item.getAsFile();
        if (file) { uploadFile(file); break; }
      }
    }
  });

  function setUploading(pct) {
    dropZone.classList.remove('success', 'error-state', 'drag-over');
    dropZone.classList.add('uploading');
    var txt = pct !== null ? 'UPLOADING... ' + pct + '%' : 'UPLOADING...';
    dropMain.textContent = txt;
    dropSub.textContent = '';
    liveRegion.textContent = txt;
  }

  function setSuccess(url) {
    dropZone.classList.remove('uploading', 'error-state', 'drag-over');
    dropZone.classList.add('success');
    dropMain.innerHTML = '';
    dropSub.innerHTML = '';

    var urlEl = document.createElement('div');
    urlEl.className = 'drop-url';
    urlEl.textContent = url;
    urlEl.title = 'Click to copy';
    urlEl.addEventListener('click', function(e) {
      e.stopPropagation();
      navigator.clipboard && navigator.clipboard.writeText(url).then(function() {
        urlEl.textContent = 'COPIED';
        setTimeout(function() { urlEl.textContent = url; }, 1200);
      });
    });

    var anotherBtn = document.createElement('button');
    anotherBtn.className = 'drop-another';
    anotherBtn.textContent = 'upload another';
    anotherBtn.addEventListener('click', function(e) {
      e.stopPropagation();
      resetZone();
    });

    dropMain.appendChild(urlEl);
    dropSub.appendChild(anotherBtn);
    liveRegion.textContent = 'Upload complete. URL: ' + url;
  }

  function setError(msg) {
    dropZone.classList.remove('uploading', 'success', 'drag-over');
    dropZone.classList.add('error-state');
    dropMain.textContent = msg || 'UPLOAD FAILED';
    dropSub.textContent = '';
    liveRegion.textContent = msg || 'Upload failed';
    setTimeout(resetZone, 3000);
  }

  function resetZone() {
    dropZone.classList.remove('uploading', 'success', 'error-state', 'drag-over');
    dropMain.textContent = 'DROP FILE HERE';
    dropSub.textContent = 'click to browse \u2022 ctrl+v to paste \u2022 drag \u0026 drop';
    liveRegion.textContent = '';
    fileInput.value = '';
  }

  function uploadFile(file) {
    setUploading(0);
    var fd = new FormData();
    fd.append('file', file);
    if (pwCheck.checked && pwInput.value) {
      fd.append('password', pwInput.value);
    }
    var xhr = new XMLHttpRequest();
    xhr.open('POST', '/');
    xhr.upload.onprogress = function(e) {
      if (e.lengthComputable) {
        var pct = Math.round(e.loaded / e.total * 100);
        setUploading(pct);
      }
    };
    xhr.onload = function() {
      if (xhr.status === 200) {
        var url = xhr.responseText.trim();
        setSuccess(url);
      } else if (xhr.status === 413) {
        setError('FILE TOO LARGE');
      } else {
        setError('UPLOAD FAILED: ' + xhr.status);
      }
    };
    xhr.onerror = function() { setError('NETWORK ERROR'); };
    xhr.send(fd);
  }
})();
</script>
</body>
</html>`

var indexTpl = template.Must(template.New("index").Parse(indexHTML))

// ---- Handlers --------------------------------------------------------------

type indexData struct {
	BgURL string
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	bgURL := pickRandomBg()
	w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTpl.Execute(w, indexData{BgURL: bgURL}); err != nil {
		log.Printf("index template: %v", err)
	}
}

func pickRandomBg() string {
	entries, err := os.ReadDir(conf.Bgfolder)
	if err != nil || len(entries) == 0 {
		return ""
	}
	var gifs []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".gif") {
			gifs = append(gifs, e.Name())
		}
	}
	if len(gifs) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(gifs))))
	if err != nil {
		return "/bg/" + html.EscapeString(gifs[0])
	}
	return "/bg/" + html.EscapeString(gifs[n.Int64()])
}

func handleUpload(w http.ResponseWriter, r *http.Request) {
	// Rate limit by IP
	ip := r.RemoteAddr
	if idx := strings.LastIndex(ip, ":"); idx != -1 {
		ip = ip[:idx]
	}
	if !rateLimiter.Allow(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}

	// Enforce size limit
	r.Body = http.MaxBytesReader(w, r.Body, conf.MaxUploadMB*1024*1024)

	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
		return
	}

	var origFilename string
	var password string
	var sizeExceeded bool
	var tempPath string

	// Ensure temp file is cleaned up on any error path.
	defer func() {
		if tempPath != "" {
			os.Remove(tempPath)
		}
	}()

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if strings.Contains(err.Error(), "request body too large") || strings.Contains(err.Error(), "http: request body too large") {
				sizeExceeded = true
				break
			}
			http.Error(w, "Bad Request: "+err.Error(), http.StatusBadRequest)
			return
		}

		fieldName := part.FormName()
		if fieldName == "password" {
			pwBytes, err := io.ReadAll(io.LimitReader(part, 1024))
			if err == nil {
				password = string(pwBytes)
			}
			part.Close()
			continue
		}
		if fieldName == "file" {
			origFilename = part.FileName()
			// Stream directly to a temp file — never buffer in memory.
			tmpFile, err := os.CreateTemp(conf.Folder, ".upload-*")
			if err != nil {
				part.Close()
				http.Error(w, "Storage error", http.StatusInternalServerError)
				return
			}
			tempPath = tmpFile.Name()
			_, copyErr := io.Copy(tmpFile, part)
			tmpFile.Close()
			part.Close()
			if copyErr != nil {
				if strings.Contains(copyErr.Error(), "request body too large") || strings.Contains(copyErr.Error(), "http: request body too large") {
					sizeExceeded = true
					break
				}
				http.Error(w, "Read error", http.StatusInternalServerError)
				return
			}
		} else {
			part.Close()
		}
	}

	if sizeExceeded {
		http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
		return
	}

	if tempPath == "" {
		http.Error(w, "No file provided", http.StatusBadRequest)
		return
	}

	// Verify the temp file has content.
	fi, err := os.Stat(tempPath)
	if err != nil || fi.Size() == 0 {
		http.Error(w, "No file provided", http.StatusBadRequest)
		return
	}

	// Detect MIME type from the file on disk — no memory buffering.
	mtype, err := mimetype.DetectFile(tempPath)
	if err != nil {
		http.Error(w, "MIME detection failed", http.StatusInternalServerError)
		return
	}
	ext := mtype.Extension()
	if ext == "" {
		// Fall back to original extension if mime gives nothing.
		if origFilename != "" {
			ext = filepath.Ext(origFilename)
		}
		if ext == "" {
			ext = ".bin"
		}
	}

	// Generate unique ID and atomically rename temp file to final path.
	var finalPath string
	var finalName string
	for attempts := 0; attempts < 10; attempts++ {
		id, err := genID(conf.Filelen)
		if err != nil {
			http.Error(w, "ID generation failed", http.StatusInternalServerError)
			return
		}
		name := id + ext
		path := filepath.Join(conf.Folder, name)
		// Check for collision before rename.
		if _, statErr := os.Stat(path); statErr == nil {
			continue // file exists, retry
		}
		if err := os.Rename(tempPath, path); err != nil {
			http.Error(w, "Storage error", http.StatusInternalServerError)
			return
		}
		// Rename succeeded — clear tempPath so defer does not remove it.
		tempPath = ""
		finalPath = path
		finalName = name
		break
	}

	if finalPath == "" {
		http.Error(w, "Could not generate unique ID", http.StatusInternalServerError)
		return
	}

	// Write .meta sidecar if password provided.
	if password != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			os.Remove(finalPath)
			http.Error(w, "Password hashing failed", http.StatusInternalServerError)
			return
		}
		meta := &FileMeta{PasswordHash: string(hash)}
		if err := writeMeta(finalPath+".meta", meta); err != nil {
			os.Remove(finalPath)
			http.Error(w, "Meta write failed", http.StatusInternalServerError)
			return
		}
	}

	fileSize := fi.Size()
	_ = origFilename // used for ext fallback only
	url := "https://" + conf.Vhost + "/f/" + finalName
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(url + "\n"))
	log.Printf("upload: %s (%s, %d bytes)", finalName, mtype.String(), fileSize)
}

type pwPageData struct {
	BgURL    string
	FileID   string
	Filename string
	Error    string
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	// Extract {id} from /f/{id}
	idPart := strings.TrimPrefix(r.URL.Path, "/f/")
	if idPart == "" || strings.Contains(idPart, "/") {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Path traversal prevention
	safeName := filepath.Base(idPart)
	filePath := filepath.Join(conf.Folder, safeName)
	if !strings.HasPrefix(filePath, filepath.Clean(conf.Folder)+string(os.PathSeparator)) {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}

	// Verify file exists
	if _, err := os.Stat(filePath); err != nil {
		http.NotFound(w, r)
		return
	}

	metaPath := filePath + ".meta"
	hasMeta := false
	var meta *FileMeta
	if _, err := os.Stat(metaPath); err == nil {
		hasMeta = true
		m, err := readMeta(metaPath)
		if err != nil {
			// Corrupt .meta: fail open, log and serve
			log.Printf("download: corrupt .meta for %s: %v", safeName, err)
			hasMeta = false
		} else {
			meta = m
		}
	}

	if r.Method == http.MethodGet {
		if hasMeta && meta != nil {
			// Serve password prompt
			bgURL := pickRandomBg()
			w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			passwordTpl.Execute(w, pwPageData{
				BgURL:    bgURL,
				FileID:   safeName,
				Filename: safeName,
				Error:    "",
			})
			return
		}
		// Serve file directly
		serveFile(w, r, filePath, safeName)
		return
	}

	if r.Method == http.MethodPost {
		if !hasMeta || meta == nil {
			// No password needed — serve directly
			serveFile(w, r, filePath, safeName)
			return
		}
		r.ParseForm()
		pw := r.FormValue("password")
		if err := bcrypt.CompareHashAndPassword([]byte(meta.PasswordHash), []byte(pw)); err != nil {
			// Wrong password
			bgURL := pickRandomBg()
			w.Header().Set("Content-Security-Policy", "default-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			passwordTpl.Execute(w, pwPageData{
				BgURL:    bgURL,
				FileID:   safeName,
				Filename: safeName,
				Error:    "WRONG PASSWORD",
			})
			return
		}
		// Correct password — serve file
		serveFile(w, r, filePath, safeName)
		return
	}

	http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
}

func serveFile(w http.ResponseWriter, r *http.Request, filePath, name string) {
	activeFiles.Store(name, struct{}{})
	defer activeFiles.Delete(name)

	// Detect MIME for serving
	data, err := os.ReadFile(filePath)
	if err != nil {
		http.Error(w, "File read error", http.StatusInternalServerError)
		return
	}
	mtype := mimetype.Detect(data)
	mimeStr := mtype.String()
	// Strip params for comparison
	mediaType, _, _ := mime.ParseMediaType(mimeStr)
	if mediaType == "" {
		mediaType = mimeStr
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", mimeStr)

	if isInlineMIME(mediaType) {
		w.Header().Set("Content-Disposition", "inline; filename=\""+name+"\"")
	} else {
		w.Header().Set("Content-Disposition", "attachment; filename=\""+name+"\"")
	}

	http.ServeContent(w, r, name, time.Time{}, strings.NewReader(string(data)))
}

func handleBg(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/bg/")
	path, ok := safePath(conf.Bgfolder, name)
	if !ok {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

func handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	path, ok := safePath(conf.Webroot, name)
	if !ok {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, path)
}

// ---- Router ----------------------------------------------------------------

func newMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleUpload(w, r)
		} else {
			handleIndex(w, r)
		}
	})
	mux.HandleFunc("/f/", handleDownload)
	mux.HandleFunc("/bg/", handleBg)
	mux.HandleFunc("/static/", handleStatic)
	return mux
}

// ---- Main ------------------------------------------------------------------

func main() {
	if _, err := toml.DecodeFile("config.toml", &conf); err != nil {
		log.Fatalf("config: %v", err)
	}

	// Ensure upload dir exists
	if err := os.MkdirAll(conf.Folder, 0755); err != nil {
		log.Fatalf("mkdir %s: %v", conf.Folder, err)
	}

	rateLimiter = NewRateLimiter(conf.RateLimitPerMin)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go cullLoop(ctx)
	go rateLimiter.cleanup(ctx)

	srv := &http.Server{
		Addr:         ":" + conf.Lport,
		Handler:      newMux(),
		ReadTimeout:  5 * time.Minute,
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  2 * time.Minute,
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	go func() {
		log.Printf("hardfiles listening on :%s", conf.Lport)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	<-sigCh
	log.Println("shutting down...")
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutCancel()
	srv.Shutdown(shutCtx)
	log.Println("done")
}
