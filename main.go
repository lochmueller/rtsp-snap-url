package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

type Stream struct {
	URL       string `yaml:"url"`
	TTL       int    `yaml:"ttl"`
	Transport string `yaml:"transport"`
	Archive   int    `yaml:"archive"`  // 0=none, -1=unlimited, N=keep last N
	Interval  int    `yaml:"interval"` // seconds between auto-snapshots, 0=disabled
}

type Config struct {
	DefaultTTL       int               `yaml:"default_ttl"`
	DefaultTransport string            `yaml:"default_transport"`
	Streams          map[string]Stream `yaml:"streams"`
}

var (
	configPath string
	cacheDir   string
	ffTimeout  time.Duration
	ffmpegPath string

	configMu    sync.RWMutex
	configData  Config
	configMtime time.Time

	keyLocks         sync.Map
	keyRegex         = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.jpg$`)
	archivePathRegex = regexp.MustCompile(`^/archive/([A-Za-z0-9_-]+)(?:/([0-9A-Za-z._-]+\.jpg))?/?$`)
)

func loadConfig() (Config, error) {
	info, err := os.Stat(configPath)
	if err != nil {
		return Config{}, err
	}

	configMu.RLock()
	if !configMtime.IsZero() && info.ModTime().Equal(configMtime) {
		c := configData
		configMu.RUnlock()
		return c, nil
	}
	configMu.RUnlock()

	configMu.Lock()
	defer configMu.Unlock()
	info, err = os.Stat(configPath)
	if err != nil {
		return Config{}, err
	}
	if !configMtime.IsZero() && info.ModTime().Equal(configMtime) {
		return configData, nil
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return Config{}, fmt.Errorf("yaml: %w", err)
	}
	configData = c
	configMtime = info.ModTime()
	log.Printf("Loaded config: %d streams", len(c.Streams))
	return c, nil
}

func lockFor(key string) *sync.Mutex {
	m, _ := keyLocks.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func fresh(p string, ttl time.Duration) bool {
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < ttl
}

func grab(ctx context.Context, url, transport, dest string) error {
	// -timeout (RTSP demuxer, microseconds) makes ffmpeg bail cleanly on
	// stalled RTSP sockets rather than waiting for the parent context to
	// kill it. Replaces the deprecated -stimeout flag.
	timeoutUs := strconv.FormatInt(ffTimeout.Microseconds(), 10)
	cmd := exec.CommandContext(ctx, ffmpegPath, "-y",
		"-rtsp_transport", transport,
		"-timeout", timeoutUs,
		"-i", url,
		"-frames:v", "1",
		"-q:v", "2",
		"-f", "image2",
		dest,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(dest)
		tail := out
		if len(tail) > 800 {
			tail = tail[len(tail)-800:]
		}
		return fmt.Errorf("ffmpeg: %w\n%s", err, string(tail))
	}
	if _, err := os.Stat(dest); err != nil {
		return errors.New("ffmpeg produced no output")
	}
	return nil
}

func resolveTransport(stream Stream, cfg Config) string {
	if stream.Transport != "" {
		return stream.Transport
	}
	if cfg.DefaultTransport != "" {
		return cfg.DefaultTransport
	}
	return "tcp"
}

func resolveTTL(stream Stream, cfg Config) time.Duration {
	ttlSec := stream.TTL
	if ttlSec <= 0 {
		ttlSec = cfg.DefaultTTL
	}
	if ttlSec <= 0 {
		ttlSec = 30
	}
	return time.Duration(ttlSec) * time.Second
}

// refreshSnapshot grabs a fresh frame, archives the previous cache entry
// (if archive is enabled), then atomically replaces the cache file. The
// caller is responsible for holding lockFor(key).
func refreshSnapshot(key string, stream Stream, cfg Config) error {
	cachePath := filepath.Join(cacheDir, key+".jpg")
	tmpPath := cachePath + ".tmp"

	ctx, cancel := context.WithTimeout(context.Background(), ffTimeout)
	defer cancel()
	if err := grab(ctx, stream.URL, resolveTransport(stream, cfg), tmpPath); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return context.DeadlineExceeded
		}
		return err
	}
	archiveOld(key, cachePath, stream.Archive)
	return os.Rename(tmpPath, cachePath)
}

func archiveOld(key, cachePath string, keep int) {
	if keep == 0 {
		return
	}
	info, err := os.Stat(cachePath)
	if err != nil {
		return // nothing to archive
	}
	dir := filepath.Join(cacheDir, "archive", key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("archive mkdir %s: %v", key, err)
		return
	}
	ts := info.ModTime().UTC().Format("2006-01-02_15-04-05.000Z")
	dest := filepath.Join(dir, ts+".jpg")
	if err := os.Rename(cachePath, dest); err != nil {
		// cross-filesystem? fall back to copy.
		if data, readErr := os.ReadFile(cachePath); readErr == nil {
			if writeErr := os.WriteFile(dest, data, 0o644); writeErr != nil {
				log.Printf("archive write %s: %v", key, writeErr)
				return
			}
			_ = os.Remove(cachePath)
		} else {
			log.Printf("archive move %s: %v", key, err)
			return
		}
	}
	if keep > 0 {
		rotateArchive(dir, keep)
	}
}

func rotateArchive(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	jpgs := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".jpg") {
			jpgs = append(jpgs, e.Name())
		}
	}
	if len(jpgs) <= keep {
		return
	}
	sort.Strings(jpgs)
	for _, name := range jpgs[:len(jpgs)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// autoScheduler periodically scans the config and triggers a refresh for
// any stream whose `interval` has elapsed since its last auto-snapshot.
func autoScheduler(ctx context.Context) {
	last := map[string]time.Time{}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cfg, err := loadConfig()
			if err != nil {
				continue
			}
			now := time.Now()
			for key, stream := range cfg.Streams {
				if stream.Interval <= 0 || stream.URL == "" {
					continue
				}
				interval := time.Duration(stream.Interval) * time.Second
				if now.Sub(last[key]) < interval {
					continue
				}
				last[key] = now
				k, s, c := key, stream, cfg
				go func() {
					mu := lockFor(k)
					mu.Lock()
					defer mu.Unlock()
					if err := refreshSnapshot(k, s, c); err != nil {
						log.Printf("auto-snapshot %s: %v", k, err)
					}
				}()
			}
		}
	}
}

func serveImage(w http.ResponseWriter, r *http.Request, p string, ttl time.Duration) {
	f, err := os.Open(p)
	if err != nil {
		http.Error(w, "cannot open snapshot", http.StatusBadGateway)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		http.Error(w, "cannot stat snapshot", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(ttl.Seconds())))
	http.ServeContent(w, r, filepath.Base(p), info.ModTime(), f)
}

func snapshotHandler(w http.ResponseWriter, r *http.Request) {
	m := keyRegex.FindStringSubmatch(path.Base(r.URL.Path))
	if m == nil {
		http.NotFound(w, r)
		return
	}
	key := m[1]

	cfg, err := loadConfig()
	if err != nil {
		log.Printf("config error: %v", err)
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	stream, ok := cfg.Streams[key]
	if !ok {
		http.Error(w, "unknown stream key: "+key, http.StatusNotFound)
		return
	}
	if stream.URL == "" {
		http.Error(w, "stream has no url", http.StatusInternalServerError)
		return
	}

	ttl := resolveTTL(stream, cfg)
	cachePath := filepath.Join(cacheDir, key+".jpg")
	force := r.URL.Query().Get("refresh") == "1"
	if !force && fresh(cachePath, ttl) {
		serveImage(w, r, cachePath, ttl)
		return
	}

	mu := lockFor(key)
	mu.Lock()
	defer mu.Unlock()
	if !force && fresh(cachePath, ttl) {
		serveImage(w, r, cachePath, ttl)
		return
	}

	if err := refreshSnapshot(key, stream, cfg); err != nil {
		log.Printf("grab failed for %s: %v", key, err)
		if _, statErr := os.Stat(cachePath); statErr == nil {
			serveImage(w, r, cachePath, ttl)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "ffmpeg timed out", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "ffmpeg failed", http.StatusBadGateway)
		return
	}
	serveImage(w, r, cachePath, ttl)
}

const indexTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>rtsp-snap-url</title>
<meta http-equiv="refresh" content="5; url=/">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: -apple-system, system-ui, sans-serif; background:#111; color:#ddd; margin: 1rem; }
  h1 { font-weight: 400; font-size: 1.1rem; color:#999; }
  .toolbar { margin: 0.4rem 0 1rem; font-size: 0.85rem; }
  .toolbar a { color: #6cf; text-decoration: none; margin-right: 1rem; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 1rem; }
  .card { background:#1c1c1c; border-radius: 6px; padding: 0.4rem; }
  .card a { color: inherit; text-decoration: none; }
  .card img { width: 100%%; aspect-ratio: 16/9; object-fit: cover; background:#000; display:block; }
  .card .name { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; padding: 0.4rem 0.2rem 0.1rem; font-size: 0.9rem; display:flex; justify-content:space-between; align-items:center; gap: 0.5rem; }
  .card .name a.archive { color:#6cf; font-size: 0.75rem; }
  .empty { color:#777; }
</style>
</head>
<body>
<h1>rtsp-snap-url &mdash; %d stream%s</h1>
<div class="toolbar"><a href="/?refresh=1">Refresh all</a></div>
%s
</body>
</html>
`

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cfg, err := loadConfig()
	if err != nil {
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	keys := make([]string, 0, len(cfg.Streams))
	for k := range cfg.Streams {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	force := r.URL.Query().Get("refresh") == "1"
	imgQS := ""
	if force {
		imgQS = "?refresh=1"
	}

	var body string
	if len(keys) == 0 {
		body = `<p class="empty">No streams configured.</p>`
	} else {
		body = `<div class="grid">`
		for _, k := range keys {
			esc := html.EscapeString(k)
			archiveLink := ""
			if cfg.Streams[k].Archive != 0 {
				archiveLink = fmt.Sprintf(`<a class="archive" href="/archive/%s">archive</a>`, esc)
			}
			body += fmt.Sprintf(
				`<div class="card"><a href="/%s.jpg%s"><img loading="lazy" src="/%s.jpg%s" alt="%s"></a><div class="name"><span>%s</span>%s</div></div>`,
				esc, imgQS, esc, imgQS, esc, esc, archiveLink,
			)
		}
		body += `</div>`
	}

	plural := "s"
	if len(keys) == 1 {
		plural = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, indexTmpl, len(keys), plural, body)
}

const archiveIndexTmpl = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Archive: %s</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: -apple-system, system-ui, sans-serif; background:#111; color:#ddd; margin: 1rem; }
  h1 { font-weight: 400; font-size: 1.1rem; color:#999; }
  .back { font-size: 0.85rem; margin-bottom: 1rem; }
  .back a { color: #6cf; text-decoration: none; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(220px, 1fr)); gap: 0.8rem; }
  .card { background:#1c1c1c; border-radius: 6px; padding: 0.3rem; }
  .card a { color: inherit; text-decoration: none; }
  .card img { width: 100%%; aspect-ratio: 16/9; object-fit: cover; background:#000; display:block; }
  .card .ts { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; padding: 0.3rem 0.2rem 0.1rem; font-size: 0.75rem; color: #aaa; }
  .empty { color:#777; }
</style>
</head>
<body>
<div class="back"><a href="/">&larr; back</a></div>
<h1>archive &mdash; %s (%d snapshot%s)</h1>
%s
</body>
</html>
`

func archiveHandler(w http.ResponseWriter, r *http.Request) {
	m := archivePathRegex.FindStringSubmatch(r.URL.Path)
	if m == nil {
		http.NotFound(w, r)
		return
	}
	key, file := m[1], m[2]

	cfg, err := loadConfig()
	if err != nil {
		http.Error(w, "config error", http.StatusInternalServerError)
		return
	}
	stream, ok := cfg.Streams[key]
	if !ok || stream.Archive == 0 {
		http.NotFound(w, r)
		return
	}

	archiveDir := filepath.Join(cacheDir, "archive", key)

	if file == "" {
		renderArchiveIndex(w, key, archiveDir)
		return
	}

	// defense-in-depth against path traversal: filepath.Base must equal file
	if filepath.Base(file) != file {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(archiveDir, file)
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	http.ServeContent(w, r, file, info.ModTime(), f)
}

func renderArchiveIndex(w http.ResponseWriter, key, dir string) {
	entries, _ := os.ReadDir(dir)
	type item struct {
		Name string
		TS   time.Time
	}
	items := make([]item, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jpg") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		items = append(items, item{Name: e.Name(), TS: info.ModTime()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].TS.After(items[j].TS) })

	esc := html.EscapeString(key)
	var body string
	if len(items) == 0 {
		body = `<p class="empty">No archived snapshots yet.</p>`
	} else {
		body = `<div class="grid">`
		for _, it := range items {
			n := html.EscapeString(it.Name)
			ts := html.EscapeString(it.TS.UTC().Format("2006-01-02 15:04:05 UTC"))
			body += fmt.Sprintf(
				`<div class="card"><a href="/archive/%s/%s"><img loading="lazy" src="/archive/%s/%s" alt="%s"><div class="ts">%s</div></a></div>`,
				esc, n, esc, n, n, ts,
			)
		}
		body += `</div>`
	}

	plural := "s"
	if len(items) == 1 {
		plural = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, archiveIndexTmpl, esc, esc, len(items), plural, body)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := loadConfig(); err != nil {
		http.Error(w, "config error: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	if _, err := os.Stat(ffmpegPath); err != nil {
		http.Error(w, "ffmpeg missing: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	probe := filepath.Join(cacheDir, ".healthcheck")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		http.Error(w, "cache dir not writable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	_ = os.Remove(probe)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/" {
		indexHandler(w, r)
		return
	}
	snapshotHandler(w, r)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envOrInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func main() {
	var bind string
	var port, timeoutSec int
	flag.StringVar(&configPath, "config", envOr("RTSP_CONFIG", "/etc/rtsp-snap/config.yaml"), "config path")
	flag.StringVar(&cacheDir, "cache", envOr("RTSP_CACHE_DIR", "/var/cache/rtsp-snap"), "cache directory")
	flag.StringVar(&bind, "bind", envOr("BIND", "0.0.0.0"), "bind address")
	flag.IntVar(&port, "port", envOrInt("PORT", 8080), "listen port")
	flag.IntVar(&timeoutSec, "ffmpeg-timeout", envOrInt("RTSP_FFMPEG_TIMEOUT", 15), "ffmpeg timeout (seconds)")
	flag.Parse()
	ffTimeout = time.Duration(timeoutSec) * time.Second

	if _, err := loadConfig(); err != nil {
		log.Fatalf("load config: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Fatalf("cache dir: %v", err)
	}
	resolved, err := exec.LookPath("ffmpeg")
	if err != nil {
		log.Fatalf("ffmpeg not found in PATH: %v", err)
	}
	ffmpegPath = resolved

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/archive/", archiveHandler)
	mux.HandleFunc("/", rootHandler)

	addr := fmt.Sprintf("%s:%d", bind, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go autoScheduler(ctx)

	serverErr := make(chan error, 1)
	go func() {
		log.Printf("Listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
		}
		close(serverErr)
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			log.Fatalf("server: %v", err)
		}
	case <-ctx.Done():
		log.Println("Shutdown signal received, draining...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}
}
