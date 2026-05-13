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

	configMu    sync.RWMutex
	configData  Config
	configMtime time.Time

	keyLocks sync.Map
	keyRegex = regexp.MustCompile(`^([A-Za-z0-9_-]+)\.jpg$`)
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
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y",
		"-rtsp_transport", transport,
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
	if fresh(cachePath, ttl) {
		serveImage(w, r, cachePath, ttl)
		return
	}

	mu := lockFor(key)
	mu.Lock()
	defer mu.Unlock()
	if fresh(cachePath, ttl) {
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
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
  body { font-family: -apple-system, system-ui, sans-serif; background:#111; color:#ddd; margin: 1rem; }
  h1 { font-weight: 400; font-size: 1.1rem; color:#999; }
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 1rem; }
  .card { background:#1c1c1c; border-radius: 6px; padding: 0.4rem; }
  .card a { color: inherit; text-decoration: none; }
  .card img { width: 100%%; aspect-ratio: 16/9; object-fit: cover; background:#000; display:block; }
  .card .name { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; padding: 0.4rem 0.2rem 0.1rem; font-size: 0.9rem; }
  .empty { color:#777; }
</style>
</head>
<body>
<h1>rtsp-snap-url &mdash; %d stream%s</h1>
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

	var body string
	if len(keys) == 0 {
		body = `<p class="empty">No streams configured.</p>`
	} else {
		body = `<div class="grid">`
		for _, k := range keys {
			esc := html.EscapeString(k)
			body += fmt.Sprintf(
				`<div class="card"><a href="/%s.jpg"><img loading="lazy" src="/%s.jpg" alt="%s"><div class="name">%s</div></a></div>`,
				esc, esc, esc, esc,
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

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := loadConfig(); err != nil {
		http.Error(w, "config error", http.StatusServiceUnavailable)
		return
	}
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

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
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
