package authz

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cerbos/cerbos-sdk-go/cerbos"
)

// ── fakeBackend: lets us simulate any Cerbos response without a live PDP. ──

type fakeBackend struct {
	// IsAllowed: per-action map of allowed bools. Missing actions default to false.
	allowedActions map[string]bool

	// CheckResources: per-action map for the batched call.
	batchAllowed map[string]bool

	// Error to return from IsAllowed and CheckResources. nil → success.
	err error

	// Counters.
	isAllowedCalls   atomic.Int64
	checkResCalls    atomic.Int64
	serverInfoCalls  atomic.Int64
	serverInfoErr    error

	// Captured arguments from the most recent call.
	lastPrincipal *cerbos.Principal
	lastResource  *cerbos.Resource
	lastAction    string
	lastReqOpts   []cerbos.RequestOpt
}

func (f *fakeBackend) IsAllowed(ctx context.Context, p *cerbos.Principal, r *cerbos.Resource, action string, opts ...cerbos.RequestOpt) (bool, error) {
	f.isAllowedCalls.Add(1)
	f.lastPrincipal = p
	f.lastResource = r
	f.lastAction = action
	f.lastReqOpts = opts
	if f.err != nil {
		return false, f.err
	}
	return f.allowedActions[action], nil
}

func (f *fakeBackend) CheckResources(ctx context.Context, p *cerbos.Principal, batch *cerbos.ResourceBatch, opts ...cerbos.RequestOpt) (*cerbos.CheckResourcesResponse, error) {
	f.checkResCalls.Add(1)
	f.lastReqOpts = opts
	if f.err != nil {
		return nil, f.err
	}
	// Building a real CheckResourcesResponse without a Cerbos server is
	// awkward (the protobuf shape has many required fields). For these tests
	// we exercise the error path (returning non-nil err) and the bypass path;
	// the success path of CheckActions is exercised via a contract test in
	// the integration suite (lands once we have a Cerbos test fixture).
	return nil, errors.New("fake CheckResources success path is not implemented; use err to test failure paths")
}

func (f *fakeBackend) ServerInfo(ctx context.Context) (*cerbos.ServerInfo, error) {
	f.serverInfoCalls.Add(1)
	if f.serverInfoErr != nil {
		return nil, f.serverInfoErr
	}
	return &cerbos.ServerInfo{}, nil
}

// helper: build a Client with a fake backend.
func newTestClient(t *testing.T, backend cerbosBackend, bypass bool) (*Client, *bytes.Buffer) {
	t.Helper()
	logBuf := &bytes.Buffer{}
	return &Client{
		backend:    backend,
		bypassMode: bypass,
		envName:    "dev",
		logger:     slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}, logBuf
}

// ── Reason String ───────────────────────────────────────────────────

func TestReason_String(t *testing.T) {
	cases := map[Reason]string{
		Allowed:     "allowed",
		Denied:      "denied",
		Unreachable: "unreachable",
		Bypassed:    "bypassed",
		Reason(99):  "unknown",
	}
	for r, want := range cases {
		if got := r.String(); got != want {
			t.Errorf("Reason(%d).String() = %q; want %q", r, got, want)
		}
	}
}

// ── New: bypass-in-production safety ────────────────────────────────

func TestNew_BypassInProductionRejected(t *testing.T) {
	t.Setenv("CERBOS_ALLOW_BYPASS", "1")
	_, err := New(context.Background(), Options{
		Address: "cerbos:3593",
		EnvName: "production",
	})
	if !errors.Is(err, ErrBypassInProduction) {
		t.Errorf("err = %v; want ErrBypassInProduction", err)
	}
}

func TestNew_BypassInDevAllowed(t *testing.T) {
	t.Setenv("CERBOS_ALLOW_BYPASS", "1")
	c, err := New(context.Background(), Options{
		Address: "cerbos:3593", // unreachable, but cerbos.New only returns config errors here
		EnvName: "dev",
	})
	if err != nil {
		t.Fatalf("dev bypass: err = %v; want nil", err)
	}
	if !c.bypassMode {
		t.Errorf("bypassMode = false; want true")
	}
}

func TestNew_NoBypassWhenEnvUnset(t *testing.T) {
	// CERBOS_ALLOW_BYPASS unset → bypass is off regardless of EnvName.
	t.Setenv("CERBOS_ALLOW_BYPASS", "")
	c, err := New(context.Background(), Options{Address: "cerbos:3593", EnvName: "production"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if c.bypassMode {
		t.Errorf("bypassMode = true; want false (env var was unset)")
	}
}

func TestNew_RequiresAddress(t *testing.T) {
	_, err := New(context.Background(), Options{})
	if err == nil {
		t.Errorf("expected error when Address is empty")
	}
}

// ── CheckAction: fail-closed semantics ─────────────────────────────

func TestCheckAction_AllowedPath(t *testing.T) {
	fake := &fakeBackend{allowedActions: map[string]bool{"read": true}}
	c, _ := newTestClient(t, fake, false)

	d := c.CheckAction(context.Background(),
		Principal{ID: "u1", Roles: []string{"reader"}},
		Resource{Kind: "Item", ID: "i1"},
		"read")

	if !d.Allowed || d.Reason != Allowed {
		t.Errorf("got %+v; want allowed=true reason=Allowed", d)
	}
	if d.Action != "read" {
		t.Errorf("Action = %q; want 'read'", d.Action)
	}
	if fake.isAllowedCalls.Load() != 1 {
		t.Errorf("backend called %d times; want 1", fake.isAllowedCalls.Load())
	}
}

func TestCheckAction_DeniedPath(t *testing.T) {
	fake := &fakeBackend{allowedActions: map[string]bool{}} // nothing allowed
	c, _ := newTestClient(t, fake, false)

	d := c.CheckAction(context.Background(),
		Principal{ID: "u1"},
		Resource{Kind: "Item", ID: "i1"},
		"delete")

	if d.Allowed || d.Reason != Denied {
		t.Errorf("got %+v; want allowed=false reason=Denied", d)
	}
}

func TestCheckAction_UnreachablePath(t *testing.T) {
	fake := &fakeBackend{err: errors.New("connection refused")}
	c, logs := newTestClient(t, fake, false)

	d := c.CheckAction(context.Background(),
		Principal{ID: "u1"},
		Resource{Kind: "Item", ID: "i1"},
		"read")

	if d.Allowed {
		t.Errorf("Unreachable should be fail-closed (Allowed=false)")
	}
	if d.Reason != Unreachable {
		t.Errorf("Reason = %v; want Unreachable", d.Reason)
	}
	if !strings.Contains(logs.String(), "connection refused") {
		t.Errorf("logs should mention the underlying error; got %q", logs.String())
	}
}

func TestCheckAction_PassesAttributesToBackend(t *testing.T) {
	fake := &fakeBackend{allowedActions: map[string]bool{"read": true}}
	c, _ := newTestClient(t, fake, false)

	c.CheckAction(context.Background(),
		Principal{ID: "u1", Roles: []string{"editor", "reader"},
			Attributes: map[string]any{"team": "platform"}},
		Resource{Kind: "Item", ID: "i1",
			Attributes: map[string]any{"owner": "u2"}},
		"read")

	if fake.lastPrincipal.ID() != "u1" {
		t.Errorf("Principal.ID = %q; want u1", fake.lastPrincipal.ID())
	}
	if fake.lastResource.Kind() != "Item" {
		t.Errorf("Resource.Kind = %q; want Item", fake.lastResource.Kind())
	}
	if fake.lastResource.ID() != "i1" {
		t.Errorf("Resource.ID = %q; want i1", fake.lastResource.ID())
	}
}

// ── Bypass mode behaviour ──────────────────────────────────────────

func TestCheckAction_BypassReturnsAllowed(t *testing.T) {
	// Bypass mode short-circuits without calling the backend.
	fake := &fakeBackend{err: errors.New("would error if reached")}
	c, logs := newTestClient(t, fake, true)

	d := c.CheckAction(context.Background(),
		Principal{ID: "u1"}, Resource{Kind: "Item", ID: "i1"}, "delete")

	if !d.Allowed {
		t.Errorf("bypass should return Allowed=true; got %+v", d)
	}
	if d.Reason != Bypassed {
		t.Errorf("Reason = %v; want Bypassed", d.Reason)
	}
	if fake.isAllowedCalls.Load() != 0 {
		t.Errorf("backend was called %d times; bypass should short-circuit", fake.isAllowedCalls.Load())
	}
	if !strings.Contains(logs.String(), "BYPASS") {
		t.Errorf("bypass should log loudly; got %q", logs.String())
	}
}

func TestCheckActions_BypassReturnsAllAllowed(t *testing.T) {
	fake := &fakeBackend{}
	c, _ := newTestClient(t, fake, true)

	decisions := c.CheckActions(context.Background(),
		Principal{ID: "u1"}, Resource{Kind: "Item", ID: "i1"},
		[]string{"read", "update", "delete"})

	if len(decisions) != 3 {
		t.Errorf("got %d decisions; want 3", len(decisions))
	}
	for action, d := range decisions {
		if !d.Allowed || d.Reason != Bypassed {
			t.Errorf("decisions[%q] = %+v; want bypassed-allowed", action, d)
		}
	}
	if fake.checkResCalls.Load() != 0 {
		t.Errorf("backend.CheckResources called %d times; bypass should short-circuit", fake.checkResCalls.Load())
	}
}

func TestPermissionMap_BypassReturnsAllTrue(t *testing.T) {
	fake := &fakeBackend{}
	c, _ := newTestClient(t, fake, true)

	perms := c.PermissionMap(context.Background(),
		Principal{ID: "u1"}, Resource{Kind: "Item", ID: "i1"},
		[]string{"read", "delete"})

	if len(perms) != 2 || !perms["read"] || !perms["delete"] {
		t.Errorf("bypass map = %+v; want all true", perms)
	}
}

// ── CheckActions: failure path ─────────────────────────────────────

func TestCheckActions_UnreachableSetsAllToUnreachable(t *testing.T) {
	fake := &fakeBackend{err: errors.New("PDP down")}
	c, _ := newTestClient(t, fake, false)

	decisions := c.CheckActions(context.Background(),
		Principal{ID: "u1"}, Resource{Kind: "Item", ID: "i1"},
		[]string{"read", "update", "delete"})

	if len(decisions) != 3 {
		t.Errorf("got %d decisions; want 3", len(decisions))
	}
	for action, d := range decisions {
		if d.Allowed {
			t.Errorf("decisions[%q]: should be fail-closed; got Allowed=true", action)
		}
		if d.Reason != Unreachable {
			t.Errorf("decisions[%q].Reason = %v; want Unreachable", action, d.Reason)
		}
	}
}

func TestPermissionMap_UnreachableSetsAllFalse(t *testing.T) {
	fake := &fakeBackend{err: errors.New("PDP down")}
	c, _ := newTestClient(t, fake, false)

	perms := c.PermissionMap(context.Background(),
		Principal{ID: "u1"}, Resource{Kind: "Item", ID: "i1"},
		[]string{"read", "delete"})

	if perms["read"] || perms["delete"] {
		t.Errorf("perms = %+v; want all false (fail-closed)", perms)
	}
}

// ── Ping ────────────────────────────────────────────────────────────

func TestPing_Success(t *testing.T) {
	fake := &fakeBackend{}
	c, _ := newTestClient(t, fake, false)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping err = %v; want nil", err)
	}
	if fake.serverInfoCalls.Load() != 1 {
		t.Errorf("ServerInfo called %d times; want 1", fake.serverInfoCalls.Load())
	}
}

func TestPing_PropagatesError(t *testing.T) {
	fake := &fakeBackend{serverInfoErr: errors.New("PDP down")}
	c, _ := newTestClient(t, fake, false)
	if err := c.Ping(context.Background()); err == nil {
		t.Errorf("Ping should propagate ServerInfo error")
	}
}

func TestPing_BypassSkipsBackend(t *testing.T) {
	fake := &fakeBackend{serverInfoErr: errors.New("would error if reached")}
	c, _ := newTestClient(t, fake, true)
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("bypass Ping err = %v; want nil", err)
	}
	if fake.serverInfoCalls.Load() != 0 {
		t.Errorf("bypass should not call ServerInfo")
	}
}

// ── Close ───────────────────────────────────────────────────────────

func TestClose_NoOp(t *testing.T) {
	c, _ := newTestClient(t, &fakeBackend{}, false)
	if err := c.Close(); err != nil {
		t.Errorf("Close err = %v; want nil", err)
	}
}

// ── toCerbosPrincipal / toCerbosResource ─────────────────────────────

func TestToCerbosPrincipal_BasicFields(t *testing.T) {
	cp := toCerbosPrincipal(Principal{
		ID:    "u1",
		Roles: []string{"reader", "editor"},
		Attributes: map[string]any{
			"team": "platform",
		},
	})
	if cp.ID() != "u1" {
		t.Errorf("ID = %q; want u1", cp.ID())
	}
	if len(cp.Roles()) != 2 {
		t.Errorf("Roles = %v; want 2 entries", cp.Roles())
	}
}

func TestToCerbosResource_BasicFields(t *testing.T) {
	cr := toCerbosResource(Resource{
		Kind:       "Item",
		ID:         "i1",
		Attributes: map[string]any{"status": "active"},
	})
	if cr.Kind() != "Item" {
		t.Errorf("Kind = %q; want Item", cr.Kind())
	}
	if cr.ID() != "i1" {
		t.Errorf("ID = %q; want i1", cr.ID())
	}
}

// ── AuxData / JWT wiring ────────────────────────────────────────────

func TestCheckAction_PassesJWTViaAuxData(t *testing.T) {
	fake := &fakeBackend{allowedActions: map[string]bool{"read": true}}
	c, _ := newTestClient(t, fake, false)

	d := c.CheckAction(context.Background(),
		Principal{
			ID:      "alice",
			Roles:   []string{"editor"},
			AuxData: &AuxData{JWT: "header.payload.sig"},
		},
		Resource{Kind: "Item", ID: "i1"},
		"read",
	)
	if !d.Allowed {
		t.Fatalf("Allowed = false; want true")
	}
	if len(fake.lastReqOpts) == 0 {
		t.Fatalf("backend received no request opts; expected AuxDataJWT to be passed")
	}
}

func TestCheckAction_OmitsRequestOptsWhenNoJWT(t *testing.T) {
	fake := &fakeBackend{allowedActions: map[string]bool{"read": true}}
	c, _ := newTestClient(t, fake, false)

	c.CheckAction(context.Background(),
		Principal{ID: "alice", Roles: []string{"editor"}}, // no AuxData
		Resource{Kind: "Item", ID: "i1"},
		"read",
	)
	if len(fake.lastReqOpts) != 0 {
		t.Errorf("backend got %d request opts; expected zero when AuxData is nil", len(fake.lastReqOpts))
	}
}

// ── Logger discard helper (avoid leaking output to test runner) ─────

var _ io.Writer = (*bytes.Buffer)(nil)
