package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Stream struct {
	URL       string `yaml:"url"`
	TTL       int    `yaml:"ttl"`
	Transport string `yaml:"transport"`
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

func fresh(path string, ttl time.Duration) bool {
	info, err := os.Stat(path)
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

func serveImage(w http.ResponseWriter, r *http.Request, path string, ttl time.Duration) {
	f, err := os.Open(path)
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
	http.ServeContent(w, r, filepath.Base(path), info.ModTime(), f)
}

func handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
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

	ttlSec := stream.TTL
	if ttlSec <= 0 {
		ttlSec = cfg.DefaultTTL
	}
	if ttlSec <= 0 {
		ttlSec = 30
	}
	ttl := time.Duration(ttlSec) * time.Second

	transport := stream.Transport
	if transport == "" {
		transport = cfg.DefaultTransport
	}
	if transport == "" {
		transport = "tcp"
	}

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

	tmpPath := cachePath + ".tmp"
	ctx, cancel := context.WithTimeout(context.Background(), ffTimeout)
	defer cancel()
	if err := grab(ctx, stream.URL, transport, tmpPath); err != nil {
		log.Printf("grab failed for %s: %v", key, err)
		if _, statErr := os.Stat(cachePath); statErr == nil {
			serveImage(w, r, cachePath, ttl)
			return
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			http.Error(w, "ffmpeg timed out", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "ffmpeg failed", http.StatusBadGateway)
		return
	}
	if err := os.Rename(tmpPath, cachePath); err != nil {
		http.Error(w, "rename failed", http.StatusInternalServerError)
		return
	}
	serveImage(w, r, cachePath, ttl)
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
	mux.HandleFunc("/", handler)
	addr := fmt.Sprintf("%s:%d", bind, port)
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Listening on %s", addr)
	log.Fatal(srv.ListenAndServe())
}
