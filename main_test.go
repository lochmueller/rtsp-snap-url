package main

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestResolveTransport(t *testing.T) {
	cases := []struct {
		name   string
		stream Stream
		cfg    Config
		want   string
	}{
		{"stream override wins", Stream{Transport: "udp"}, Config{DefaultTransport: "tcp"}, "udp"},
		{"falls back to config default", Stream{}, Config{DefaultTransport: "udp"}, "udp"},
		{"hard default is tcp", Stream{}, Config{}, "tcp"},
		{"empty stream uses config", Stream{Transport: ""}, Config{DefaultTransport: "udp"}, "udp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTransport(c.stream, c.cfg); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveTTL(t *testing.T) {
	cases := []struct {
		name   string
		stream Stream
		cfg    Config
		want   time.Duration
	}{
		{"stream override wins", Stream{TTL: 10}, Config{DefaultTTL: 60}, 10 * time.Second},
		{"falls back to config default", Stream{}, Config{DefaultTTL: 60}, 60 * time.Second},
		{"hard default is 30s", Stream{}, Config{}, 30 * time.Second},
		{"zero stream TTL uses config", Stream{TTL: 0}, Config{DefaultTTL: 45}, 45 * time.Second},
		{"negative stream TTL uses config", Stream{TTL: -5}, Config{DefaultTTL: 45}, 45 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveTTL(c.stream, c.cfg); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestKeyRegex(t *testing.T) {
	cases := map[string]bool{
		"front_door.jpg":       true,
		"backyard.jpg":         true,
		"Cam-01.jpg":           true,
		"a.jpg":                true,
		"ABC_xyz-123.jpg":      true,
		"front_door":           false,
		"front_door.jpeg":      false,
		"front door.jpg":       false,
		"front.door.jpg":       false,
		"../etc/passwd.jpg":    false,
		"sub/dir/foo.jpg":      false,
		"":                     false,
		".jpg":                 false,
		"front_door.jpg.extra": false,
	}
	for input, shouldMatch := range cases {
		got := keyRegex.MatchString(input)
		if got != shouldMatch {
			t.Errorf("keyRegex.MatchString(%q) = %v, want %v", input, got, shouldMatch)
		}
	}
}

func TestArchivePathRegex(t *testing.T) {
	cases := []struct {
		path       string
		shouldOk   bool
		wantKey    string
		wantFile   string
	}{
		{"/archive/front_door", true, "front_door", ""},
		{"/archive/front_door/", true, "front_door", ""},
		{"/archive/front_door/2026-05-13_10-00-00.000Z.jpg", true, "front_door", "2026-05-13_10-00-00.000Z.jpg"},
		{"/archive/Cam-01/foo.jpg", true, "Cam-01", "foo.jpg"},
		{"/archive/", false, "", ""},
		{"/archive/front_door/sub/foo.jpg", false, "", ""},
		{"/archive/front_door/foo.png", false, "", ""},
		{"/archive/front door/foo.jpg", false, "", ""},
		{"/archive/front_door/../../etc/passwd", false, "", ""},
		{"/front_door.jpg", false, "", ""},
	}
	for _, c := range cases {
		m := archivePathRegex.FindStringSubmatch(c.path)
		if c.shouldOk {
			if m == nil {
				t.Errorf("path %q: expected match, got none", c.path)
				continue
			}
			if m[1] != c.wantKey || m[2] != c.wantFile {
				t.Errorf("path %q: got key=%q file=%q, want key=%q file=%q",
					c.path, m[1], m[2], c.wantKey, c.wantFile)
			}
		} else if m != nil {
			t.Errorf("path %q: expected no match, got %v", c.path, m)
		}
	}
}

// Even when archivePathRegex matches, the handler defends against traversal
// via filepath.Base(file) != file. Make sure that invariant holds for the
// inputs the regex *does* let through.
func TestArchiveFilenameBaseInvariant(t *testing.T) {
	allowed := []string{
		"2026-05-13_10-00-00.000Z.jpg",
		"foo.jpg",
		"a.jpg",
		"...jpg",
	}
	for _, name := range allowed {
		if !archivePathRegex.MatchString("/archive/k/" + name) {
			t.Fatalf("regex should accept %q for this test to be meaningful", name)
		}
		if filepath.Base(name) != name {
			t.Errorf("filepath.Base(%q) = %q, traversal defense would catch it (good)",
				name, filepath.Base(name))
		}
	}
}

func TestFresh(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "snap.jpg")

	if fresh(p, time.Minute) {
		t.Errorf("nonexistent file should not be fresh")
	}

	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fresh(p, time.Minute) {
		t.Errorf("just-written file should be fresh within 1m TTL")
	}

	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if fresh(p, time.Minute) {
		t.Errorf("2h-old file should not be fresh within 1m TTL")
	}
	if !fresh(p, 3*time.Hour) {
		t.Errorf("2h-old file should be fresh within 3h TTL")
	}
}

func TestRotateArchive(t *testing.T) {
	dir := t.TempDir()

	// Names sort lexicographically the same as chronologically because the
	// real format is `YYYY-MM-DD_HH-MM-SS.mmmZ.jpg` — we mimic that here.
	names := []string{
		"2026-05-13_10-00-00.000Z.jpg",
		"2026-05-13_10-01-00.000Z.jpg",
		"2026-05-13_10-02-00.000Z.jpg",
		"2026-05-13_10-03-00.000Z.jpg",
		"2026-05-13_10-04-00.000Z.jpg",
	}
	// Also seed a non-jpg and a subdirectory to make sure they're ignored.
	if err := os.WriteFile(filepath.Join(dir, "ignore.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	rotateArchive(dir, 3)

	got := listJPGs(t, dir)
	want := names[2:] // keep the 3 newest (lexicographically largest)
	sort.Strings(want)
	if !equalStrings(got, want) {
		t.Errorf("after keep=3: got %v, want %v", got, want)
	}

	// Non-jpg and subdir must still be there.
	if _, err := os.Stat(filepath.Join(dir, "ignore.txt")); err != nil {
		t.Errorf("non-jpg should not be removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "subdir")); err != nil {
		t.Errorf("subdir should not be removed: %v", err)
	}
}

func TestRotateArchiveKeepZero(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.jpg"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// keep=0 means delete all jpgs (this branch is only reached from
	// archiveOld when keep > 0; rotateArchive itself just enforces the limit).
	rotateArchive(dir, 0)
	if got := listJPGs(t, dir); len(got) != 0 {
		t.Errorf("keep=0 should leave no jpgs, got %v", got)
	}
}

func TestRotateArchiveNothingToDo(t *testing.T) {
	dir := t.TempDir()
	names := []string{"a.jpg", "b.jpg"}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rotateArchive(dir, 5)
	got := listJPGs(t, dir)
	if !equalStrings(got, names) {
		t.Errorf("got %v, want %v", got, names)
	}
}

func TestArchiveOldDisabled(t *testing.T) {
	prev := cacheDir
	t.Cleanup(func() { cacheDir = prev })
	cacheDir = t.TempDir()

	cache := filepath.Join(cacheDir, "cam.jpg")
	if err := os.WriteFile(cache, []byte("img"), 0o644); err != nil {
		t.Fatal(err)
	}

	archiveOld("cam", cache, 0)

	// archive=0 must not touch the cache file or create an archive dir.
	if _, err := os.Stat(cache); err != nil {
		t.Errorf("cache must be left intact when archive=0: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "archive", "cam")); !os.IsNotExist(err) {
		t.Errorf("archive dir must not be created when archive=0 (err=%v)", err)
	}
}

func TestArchiveOldUnlimited(t *testing.T) {
	prev := cacheDir
	t.Cleanup(func() { cacheDir = prev })
	cacheDir = t.TempDir()

	cache := filepath.Join(cacheDir, "cam.jpg")
	archiveDir := filepath.Join(cacheDir, "archive", "cam")

	// Write 3 cache frames in sequence, each archived with keep=-1 (unlimited).
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(cache, []byte("frame"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Force distinct mtimes so the timestamped archive filenames differ.
		mod := time.Now().Add(time.Duration(-i) * time.Second)
		if err := os.Chtimes(cache, mod, mod); err != nil {
			t.Fatal(err)
		}
		archiveOld("cam", cache, -1)
	}

	got := listJPGs(t, archiveDir)
	if len(got) != 3 {
		t.Errorf("expected 3 archived frames, got %d (%v)", len(got), got)
	}
}

func TestArchiveOldKeepLimit(t *testing.T) {
	prev := cacheDir
	t.Cleanup(func() { cacheDir = prev })
	cacheDir = t.TempDir()

	cache := filepath.Join(cacheDir, "cam.jpg")
	archiveDir := filepath.Join(cacheDir, "archive", "cam")

	for i := 0; i < 5; i++ {
		if err := os.WriteFile(cache, []byte("frame"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Spread mtimes so each archive file gets a unique name.
		mod := time.Date(2026, 5, 13, 10, i, 0, 0, time.UTC)
		if err := os.Chtimes(cache, mod, mod); err != nil {
			t.Fatal(err)
		}
		archiveOld("cam", cache, 2)
	}

	got := listJPGs(t, archiveDir)
	if len(got) != 2 {
		t.Errorf("expected exactly 2 archived frames (keep=2), got %d (%v)", len(got), got)
	}
}

func TestArchiveOldNoCacheFile(t *testing.T) {
	prev := cacheDir
	t.Cleanup(func() { cacheDir = prev })
	cacheDir = t.TempDir()

	// Cache file does not exist yet (first-ever snapshot). Must be a no-op.
	archiveOld("cam", filepath.Join(cacheDir, "cam.jpg"), 5)

	if _, err := os.Stat(filepath.Join(cacheDir, "archive", "cam")); !os.IsNotExist(err) {
		t.Errorf("archive dir must not be created when there's nothing to archive (err=%v)", err)
	}
}

func listJPGs(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".jpg" {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
