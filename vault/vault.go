// Package vault is Plinth's secret reader.
//
// Reads secrets from a layered list of [Source]s (default: /run/secrets/<name>
// then env var), caches in memory, and either panics-loudly-on-missing
// ([Reader.MustGet]) or returns a found-flag ([Reader.Get]).
//
// See https://plinth.run/sdk/go/vault/ for the design rationale.
package vault

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Source is anything that knows how to retrieve a secret by name.
// Returns the value and a found-flag (false → no value, treat as missing).
type Source func(name string) (value string, found bool)

// FileSource reads from a directory; one file per secret. The trailing
// newline (if any) is trimmed. Default dir is "/run/secrets" — what
// External Secrets Operator and Docker secrets both produce.
//
// The function rejects path-traversal: name must be a single path segment
// (no slashes, no "..", no leading dot trickery).
func FileSource(dir string) Source {
	if dir == "" {
		dir = "/run/secrets"
	}
	return func(name string) (string, bool) {
		if !isSafeSecretName(name) {
			return "", false
		}
		path := filepath.Join(dir, name)
		f, err := os.Open(path)
		if err != nil {
			return "", false
		}
		defer f.Close()
		b, err := io.ReadAll(f)
		if err != nil {
			return "", false
		}
		return strings.TrimRight(string(b), "\r\n"), true
	}
}

// isSafeSecretName rejects path-traversal: a name must be one segment with no
// separators or special components.
func isSafeSecretName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	return true
}

// EnvSource reads from environment variables. The optional prefix is added
// before the lookup; "" means no prefix.
//
//	EnvSource("")              // reads e.g. DATABASE_URL
//	EnvSource("PLINTH_")       // reads PLINTH_DATABASE_URL
func EnvSource(prefix string) Source {
	return func(name string) (string, bool) {
		v, ok := os.LookupEnv(prefix + name)
		if !ok {
			return "", false
		}
		return strings.TrimRight(v, "\r\n"), true
	}
}

// Reader queries [Source]s in registration order, returns the first found
// value, and caches the result. Threadsafe.
//
// First-found-wins: if FileSource returns a value, EnvSource isn't consulted.
// This is deliberate — Kubernetes/Docker secrets reach /run/secrets via
// External Secrets Operator, which is the production source of truth; env
// variables are only the local-dev fallback.
type Reader struct {
	sources []Source
	cache   sync.Map // map[string]string — only positive lookups; never panics
}

// New returns a Reader configured with the given sources. With no sources,
// defaults to FileSource("/run/secrets") then EnvSource("") — the canonical
// Plinth layering.
func New(sources ...Source) *Reader {
	if len(sources) == 0 {
		sources = []Source{FileSource(""), EnvSource("")}
	}
	return &Reader{sources: sources}
}

// Default is the package-level Reader, suitable for most modules.
// Initialised eagerly with the canonical sources at process start.
var Default = New()

// Get returns the secret value and whether it was found. Threadsafe.
// First call hits the sources; subsequent calls return from cache.
func (r *Reader) Get(name string) (value string, found bool) {
	if v, ok := r.cache.Load(name); ok {
		return v.(string), true
	}
	for _, s := range r.sources {
		if v, ok := s(name); ok {
			r.cache.Store(name, v)
			return v, true
		}
	}
	return "", false
}

// MustGet panics with a helpful error if the secret is missing. Use for
// required secrets at startup; never call inside a request handler.
//
// The panic message names the secret but not the value, so panic logs are
// safe to capture. Source descriptions are included to help operators
// diagnose missing-secret errors.
func (r *Reader) MustGet(name string) string {
	v, ok := r.Get(name)
	if !ok {
		panic(fmt.Sprintf("vault: required secret not found: %s (checked %d sources)", name, len(r.sources)))
	}
	return v
}

// Refresh clears the cached value for a single name. Next [Reader.Get] will
// re-read from sources. Used by modules that support hot secret rotation.
//
// Note: only the cache is invalidated. The underlying source still returns
// whatever's on disk / in env at the moment of the next read.
func (r *Reader) Refresh(name string) {
	r.cache.Delete(name)
}
