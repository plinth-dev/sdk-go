# `github.com/plinth-dev/sdk-go/vault`

Plinth's secret reader. Reads from a layered list of `Source`s (default: `/run/secrets/<name>` then env var), caches in memory.

Design rationale: <https://plinth.run/sdk/go/vault/>.

## Install

```bash
go get github.com/plinth-dev/sdk-go/vault@latest
```

## Minimum example

```go
import "github.com/plinth-dev/sdk-go/vault"

func main() {
    // Default reader: /run/secrets/<name> → env var fallback.
    dbURL := vault.Default.MustGet("DATABASE_URL")
    cerbosAddr := vault.Default.MustGet("CERBOS_ADDRESS")

    if vendorKey, ok := vault.Default.Get("OPTIONAL_VENDOR_KEY"); ok {
        // wire optional integration
        _ = vendorKey
    }

    // ... rest of main
}
```

For tests or non-default layering, construct a Reader explicitly:

```go
r := vault.New(
    vault.FileSource("./testdata/secrets"),
    vault.EnvSource("PLINTH_TEST_"),
)
```

## Behaviour

- **First-found wins.** Sources are queried in order; the first that returns `found=true` provides the value.
- **In-memory cache, no TTL.** Subsequent reads return the cached value. Use `Refresh(name)` to force a re-read (when the underlying source rotates).
- **Path traversal blocked.** `FileSource` rejects names containing `/`, `\`, or `..`.
- **Trailing newline trimmed.** Both `FileSource` and `EnvSource` strip a single trailing `\n` or `\r\n`.
- **No logging of values.** Ever. Errors mention only the name.
- **`MustGet` panics on missing.** Use only at startup; never inside a request handler.

## Compatibility

- **Go 1.23+**.
- **No external dependencies.** Pure standard library.

## License

MIT — see [LICENSE](./LICENSE).
