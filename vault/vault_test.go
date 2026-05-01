package vault

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// ── FileSource ───────────────────────────────────────────────────────

func TestFileSource_ReadsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DB_PASSWORD"), []byte("hunter2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := FileSource(dir)
	v, ok := src("DB_PASSWORD")
	if !ok || v != "hunter2" {
		t.Errorf("FileSource = (%q, %v); want ('hunter2', true)", v, ok)
	}
}

func TestFileSource_TrimsTrailingNewlines(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content, want string
	}{
		{"unix", "hello\n", "hello"},
		{"windows", "hello\r\n", "hello"},
		{"none", "hello", "hello"},
		{"multi", "hello\n\n\n", "hello"},
		{"with-newline-in-middle", "hello\nworld\n", "hello\nworld"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name)
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			src := FileSource(dir)
			v, ok := src(c.name)
			if !ok {
				t.Fatalf("not found")
			}
			if v != c.want {
				t.Errorf("got %q; want %q", v, c.want)
			}
		})
	}
}

func TestFileSource_MissingFile(t *testing.T) {
	dir := t.TempDir()
	src := FileSource(dir)
	_, ok := src("nonexistent")
	if ok {
		t.Errorf("missing file should return found=false")
	}
}

func TestFileSource_RejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	// Plant a real file outside the secrets dir.
	parent := filepath.Dir(dir)
	leak := filepath.Join(parent, "leak")
	_ = os.WriteFile(leak, []byte("nope"), 0o600)
	defer os.Remove(leak)

	src := FileSource(dir)
	cases := []string{"../leak", "../../etc/passwd", "subdir/secret", `..\leak`, "..", ""}
	for _, name := range cases {
		_, ok := src(name)
		if ok {
			t.Errorf("FileSource(%q) returned found=true; should reject path traversal", name)
		}
	}
}

func TestFileSource_DefaultDirIsRunSecrets(t *testing.T) {
	src := FileSource("")
	// Won't exist in test env; just verifying the default path resolves
	// without panic and returns found=false cleanly.
	_, ok := src("DB_PASSWORD")
	if ok {
		t.Errorf("expected no /run/secrets/DB_PASSWORD in test env")
	}
}

// ── EnvSource ────────────────────────────────────────────────────────

func TestEnvSource_NoPrefix(t *testing.T) {
	t.Setenv("MY_SECRET_TEST_VAR", "shhh")
	src := EnvSource("")
	v, ok := src("MY_SECRET_TEST_VAR")
	if !ok || v != "shhh" {
		t.Errorf("EnvSource = (%q, %v); want ('shhh', true)", v, ok)
	}
}

func TestEnvSource_WithPrefix(t *testing.T) {
	t.Setenv("PLINTH_DATABASE_URL", "postgres://x")
	src := EnvSource("PLINTH_")
	v, ok := src("DATABASE_URL")
	if !ok || v != "postgres://x" {
		t.Errorf("EnvSource = (%q, %v)", v, ok)
	}
}

func TestEnvSource_MissingVar(t *testing.T) {
	src := EnvSource("")
	_, ok := src("NEVER_SET_PLINTH_TEST_VAR_XYZ")
	if ok {
		t.Errorf("missing env var should return found=false")
	}
}

func TestEnvSource_TrimsTrailingNewlines(t *testing.T) {
	t.Setenv("MY_VAR_WITH_NEWLINE", "x\n")
	src := EnvSource("")
	v, _ := src("MY_VAR_WITH_NEWLINE")
	if v != "x" {
		t.Errorf("got %q; want 'x'", v)
	}
}

// ── Reader ───────────────────────────────────────────────────────────

func TestReader_FirstFoundWins(t *testing.T) {
	first := func(name string) (string, bool) { return "from-first", true }
	second := func(name string) (string, bool) { return "from-second", true }
	r := New(first, second)

	v, ok := r.Get("ANYTHING")
	if !ok || v != "from-first" {
		t.Errorf("got (%q, %v); want first source to win", v, ok)
	}
}

func TestReader_FallthroughOnMiss(t *testing.T) {
	first := func(name string) (string, bool) { return "", false }
	second := func(name string) (string, bool) { return "from-second", true }
	r := New(first, second)

	v, ok := r.Get("ANYTHING")
	if !ok || v != "from-second" {
		t.Errorf("got (%q, %v); want second source", v, ok)
	}
}

func TestReader_NotFound(t *testing.T) {
	miss := func(name string) (string, bool) { return "", false }
	r := New(miss, miss)
	if _, ok := r.Get("X"); ok {
		t.Errorf("got found=true for absent secret")
	}
}

func TestReader_CachesPositiveLookup(t *testing.T) {
	calls := 0
	src := func(name string) (string, bool) {
		calls++
		return "first-call-only", true
	}
	r := New(src)

	r.Get("X")
	r.Get("X")
	r.Get("X")

	if calls != 1 {
		t.Errorf("source called %d times; cache should have served calls 2+ (want 1 call)", calls)
	}
}

func TestReader_DefaultsToFileThenEnv(t *testing.T) {
	r := New() // no sources → use defaults
	t.Setenv("PLINTH_NEVER_USED_DEFAULT_TEST", "value")
	v, ok := r.Get("PLINTH_NEVER_USED_DEFAULT_TEST")
	if !ok || v != "value" {
		t.Errorf("default Reader didn't fall through file → env: (%q, %v)", v, ok)
	}
}

func TestReader_MustGetReturnsValue(t *testing.T) {
	r := New(func(name string) (string, bool) { return "ok", true })
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("MustGet on present secret should not panic; got %v", r)
		}
	}()
	v := r.MustGet("X")
	if v != "ok" {
		t.Errorf("got %q; want 'ok'", v)
	}
}

func TestReader_MustGetPanicsOnMissing(t *testing.T) {
	r := New(func(name string) (string, bool) { return "", false })
	defer func() {
		r := recover()
		if r == nil {
			t.Errorf("expected panic on missing secret")
		}
		msg, ok := r.(string)
		if !ok {
			t.Errorf("panic value should be a string; got %T", r)
		}
		if msg == "" {
			t.Errorf("panic message empty")
		}
	}()
	r.MustGet("MISSING")
}

func TestReader_RefreshClearsCache(t *testing.T) {
	calls := 0
	src := func(name string) (string, bool) {
		calls++
		return "v", true
	}
	r := New(src)

	r.Get("X") // call 1, cached
	r.Get("X") // cache hit
	r.Refresh("X")
	r.Get("X") // call 2

	if calls != 2 {
		t.Errorf("calls = %d; want 2 (Refresh should clear cache)", calls)
	}
}

func TestReader_ConcurrentReads(t *testing.T) {
	src := func(name string) (string, bool) { return "v", true }
	r := New(src)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				r.Get("X")
			}
		}()
	}
	wg.Wait()
	// If sync.Map were misused, -race would catch it.
}
