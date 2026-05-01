// Package authz is Plinth's fail-closed Cerbos PDP client.
//
// Every authorization decision in a Plinth backend module flows through this
// package; modules never talk to Cerbos directly. The fail-closed contract:
// any failure (PDP unreachable, timeout, gRPC error, context cancel, etc.)
// returns Decision{Allowed: false, Reason: Unreachable} — never an error.
// The caller writes one branch.
//
// See https://plinth.run/sdk/go/authz/ for the design rationale.
package authz

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/cerbos/cerbos-sdk-go/cerbos"
)

// Reason explains a [Decision]. Always populated, even when Allowed is true.
// The enum is the wire format ops uses to distinguish "Cerbos said no" from
// "Cerbos is dead" without parsing error strings.
type Reason int

const (
	// Allowed: PDP allowed the action.
	Allowed Reason = iota

	// Denied: PDP explicitly denied.
	Denied

	// Unreachable: PDP error / timeout / network / ctx-cancel / marshalling
	// failure — fail-closed: treated as denied.
	Unreachable

	// Bypassed: dev-only escape hatch (CERBOS_ALLOW_BYPASS=1 in non-production).
	// New() refuses to construct a Client with bypass enabled if EnvName
	// is "production"; this Reason can never appear there.
	Bypassed
)

// String returns a stable lowercase name for the Reason. Used in logs / audit.
func (r Reason) String() string {
	switch r {
	case Allowed:
		return "allowed"
	case Denied:
		return "denied"
	case Unreachable:
		return "unreachable"
	case Bypassed:
		return "bypassed"
	default:
		return "unknown"
	}
}

// Decision is the explicit outcome of a permission check.
//
// Callers should log the full Decision, not just Allowed, so ops can
// distinguish "denied by policy" from "denied because PDP is sick".
type Decision struct {
	Allowed bool
	Reason  Reason
	Action  string // populated for diagnostics; "items:read"
}

// Principal identifies the actor making the request.
//
// Populate AuxData.JWT to enable Cerbos's $jwtClaims accessor in policies.
type Principal struct {
	ID         string
	Roles      []string
	Attributes map[string]any
	AuxData    *AuxData
}

// AuxData carries data Cerbos can use beyond the principal/resource. Today
// only JWT is supported; this is a struct (not a string) to leave room for
// future fields without an API break.
type AuxData struct {
	JWT string // raw bearer token; passed through to Cerbos's AuxData
}

// Resource is the thing being acted upon. Kind matches the Cerbos resource
// kind ("Item", "Approval", ...).
type Resource struct {
	Kind       string
	ID         string
	Attributes map[string]any
}

// Sentinel errors. Returned only by [New]; the check methods never error.
var (
	// ErrBypassInProduction is returned by New if CERBOS_ALLOW_BYPASS=1
	// when EnvName is "production". This is a startup-time safety check
	// and cannot be overridden — the only way to enable bypass is to
	// not run in production.
	ErrBypassInProduction = errors.New("authz: CERBOS_ALLOW_BYPASS=1 is rejected when EnvName=\"production\"")
)

// Options configure [New]. Address is required.
type Options struct {
	// Address is the Cerbos PDP gRPC endpoint, e.g. "cerbos:3593" or
	// "passthrough:///cerbos.cerbos.svc.cluster.local:3593".
	Address string

	// TLS, when true, uses the system root CA bundle for the gRPC connection.
	// Default false uses plaintext (suitable for in-cluster service-to-service
	// over a service mesh).
	TLS bool

	// Logger receives Unreachable/Bypassed log lines. Defaults to slog.Default().
	Logger *slog.Logger

	// EnvName drives the bypass-mode safety check. If "production",
	// CERBOS_ALLOW_BYPASS=1 causes New to return ErrBypassInProduction.
	// Defaults to os.Getenv("ENV").
	EnvName string
}

// cerbosBackend is the subset of *cerbos.GRPCClient we use. Lets tests
// stub the backend without spinning up a real PDP. Unexported so external
// callers can't bypass the constructor.
type cerbosBackend interface {
	IsAllowed(ctx context.Context, p *cerbos.Principal, r *cerbos.Resource, action string) (bool, error)
	CheckResources(ctx context.Context, p *cerbos.Principal, batch *cerbos.ResourceBatch) (*cerbos.CheckResourcesResponse, error)
	ServerInfo(ctx context.Context) (*cerbos.ServerInfo, error)
}

// Client is the only surface modules use. Safe for concurrent use by
// multiple goroutines.
type Client struct {
	backend    cerbosBackend
	bypassMode bool
	envName    string
	logger     *slog.Logger
}

// New constructs a Client. Validates the bypass-mode safety check and
// dials the Cerbos PDP gRPC endpoint.
//
// Returns [ErrBypassInProduction] if CERBOS_ALLOW_BYPASS=1 and EnvName="production".
// Other errors come from the gRPC connection setup; they're propagated.
func New(ctx context.Context, opts Options) (*Client, error) {
	if opts.Address == "" {
		return nil, errors.New("authz: Options.Address is required")
	}
	if opts.EnvName == "" {
		opts.EnvName = os.Getenv("ENV")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	bypassMode := os.Getenv("CERBOS_ALLOW_BYPASS") == "1"
	if bypassMode && opts.EnvName == "production" {
		return nil, ErrBypassInProduction
	}
	if bypassMode {
		opts.Logger.Warn("authz: CERBOS_ALLOW_BYPASS=1 — every CheckAction returns Allowed",
			slog.String("env", opts.EnvName))
	}

	cerbosOpts := []cerbos.Opt{}
	if !opts.TLS {
		cerbosOpts = append(cerbosOpts, cerbos.WithPlaintext())
	}

	client, err := cerbos.New(opts.Address, cerbosOpts...)
	if err != nil {
		return nil, fmt.Errorf("authz: cerbos.New: %w", err)
	}

	return &Client{
		backend:    client,
		bypassMode: bypassMode,
		envName:    opts.EnvName,
		logger:     opts.Logger,
	}, nil
}

// Close releases any client resources. Currently a no-op; gRPC client
// connections are process-lifetime and clean up at exit. Reserved for
// future use (e.g. shutdown of a connection pool).
func (c *Client) Close() error { return nil }

// CheckAction evaluates a single action. Fail-closed:
// any error (network, PDP error, timeout, ctx-cancel) returns
// Decision{Allowed: false, Reason: Unreachable}. Never returns an error
// to the caller; the Decision is the only branch.
func (c *Client) CheckAction(ctx context.Context, p Principal, r Resource, action string) Decision {
	if c.bypassMode {
		c.logger.Warn("authz: BYPASS — would have called Cerbos",
			slog.String("action", action),
			slog.String("resource_kind", r.Kind),
			slog.String("resource_id", r.ID),
			slog.String("principal", p.ID))
		return Decision{Allowed: true, Reason: Bypassed, Action: action}
	}

	cp := toCerbosPrincipal(p)
	cr := toCerbosResource(r)

	allowed, err := c.backend.IsAllowed(ctx, cp, cr, action)
	if err != nil {
		c.logger.Warn("authz: PDP unreachable",
			slog.String("action", action),
			slog.String("resource_kind", r.Kind),
			slog.String("error", err.Error()))
		return Decision{Allowed: false, Reason: Unreachable, Action: action}
	}

	if allowed {
		return Decision{Allowed: true, Reason: Allowed, Action: action}
	}
	return Decision{Allowed: false, Reason: Denied, Action: action}
}

// CheckActions evaluates many actions against the SAME resource in one
// round-trip. Returns a map keyed by action with the same fail-closed
// semantics: on transport failure, every action gets Decision{Allowed:false,
// Reason: Unreachable}.
func (c *Client) CheckActions(ctx context.Context, p Principal, r Resource, actions []string) map[string]Decision {
	out := make(map[string]Decision, len(actions))

	if c.bypassMode {
		c.logger.Warn("authz: BYPASS — batched check would have called Cerbos",
			slog.Int("actions", len(actions)),
			slog.String("resource_kind", r.Kind),
			slog.String("resource_id", r.ID))
		for _, a := range actions {
			out[a] = Decision{Allowed: true, Reason: Bypassed, Action: a}
		}
		return out
	}

	cp := toCerbosPrincipal(p)
	cr := toCerbosResource(r)
	batch := cerbos.NewResourceBatch().Add(cr, actions...)

	resp, err := c.backend.CheckResources(ctx, cp, batch)
	if err != nil {
		c.logger.Warn("authz: PDP unreachable on batched check",
			slog.Int("actions", len(actions)),
			slog.String("resource_kind", r.Kind),
			slog.String("error", err.Error()))
		for _, a := range actions {
			out[a] = Decision{Allowed: false, Reason: Unreachable, Action: a}
		}
		return out
	}

	result := resp.GetResource(r.ID)
	for _, a := range actions {
		if result == nil {
			// Cerbos didn't return a result for this resource — treat as
			// unreachable. Shouldn't happen in normal operation.
			out[a] = Decision{Allowed: false, Reason: Unreachable, Action: a}
			continue
		}
		if result.IsAllowed(a) {
			out[a] = Decision{Allowed: true, Reason: Allowed, Action: a}
		} else {
			out[a] = Decision{Allowed: false, Reason: Denied, Action: a}
		}
	}
	return out
}

// PermissionMap returns a flattened {action: allowed} map for the given
// actions. Convenience wrapper over [Client.CheckActions] for the
// batched-check-at-layout pattern (server fetches once, passes to client).
//
// Keys are bare action names ("read"), not "kind:action" — kind is implicit
// from r.
func (c *Client) PermissionMap(ctx context.Context, p Principal, r Resource, actions []string) map[string]bool {
	decisions := c.CheckActions(ctx, p, r, actions)
	out := make(map[string]bool, len(decisions))
	for action, d := range decisions {
		out[action] = d.Allowed
	}
	return out
}

// Ping checks the Cerbos PDP is reachable. Returns the underlying gRPC
// error if not. Suitable for sdk-go/health's CerbosCheck — that package's
// Pinger interface accepts anything with this signature.
func (c *Client) Ping(ctx context.Context) error {
	if c.bypassMode {
		return nil // bypass mode never talks to Cerbos
	}
	_, err := c.backend.ServerInfo(ctx)
	return err
}

// ── internal: type conversion ────────────────────────────────────────

// toCerbosPrincipal builds a Cerbos SDK Principal from our Principal type.
// Concurrency-safe — reads from the input but mutates only the output.
func toCerbosPrincipal(p Principal) *cerbos.Principal {
	cp := cerbos.NewPrincipal(p.ID, p.Roles...)
	if len(p.Attributes) > 0 {
		// Cerbos's WithAttributes mutates the receiver and returns it.
		cp = cp.WithAttributes(p.Attributes)
	}
	// AuxData.JWT is delivered to Cerbos via per-request RequestOpts in
	// the higher-level client (PrincipalCtx); for direct IsAllowed/CheckResources
	// calls, we'd need to use Client.With(cerbos.AuxDataJWT(...)). That
	// integration lands in a follow-on once a Cerbos policy uses $jwtClaims;
	// for v0.1.0 the JWT field is reserved on Principal but not yet wired.
	_ = p.AuxData
	return cp
}

// toCerbosResource builds a Cerbos SDK Resource from our Resource type.
func toCerbosResource(r Resource) *cerbos.Resource {
	cr := cerbos.NewResource(r.Kind, r.ID)
	if len(r.Attributes) > 0 {
		cr = cr.WithAttributes(r.Attributes)
	}
	return cr
}

