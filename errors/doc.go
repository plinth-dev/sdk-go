// Package apperrors is Plinth's typed error vocabulary.
//
// Every Plinth backend module uses these six sentinel errors and the
// AppError type to express failure across handler / service / repository
// layers. Sentinels are matched via [errors.Is] — never string-compare.
//
// Standard import alias:
//
//	import apperrors "github.com/plinth-dev/sdk-go/errors"
//
// The package is named apperrors (not errors) to avoid a collision with
// the standard library's errors package. The import path keeps "errors"
// for symmetry with the SDK directory layout.
//
// See https://plinth.run/sdk/go/errors/ for the design rationale.
package apperrors
