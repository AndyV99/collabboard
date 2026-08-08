package realtime

// The test harness.
//
// Everything here builds the *real* stack and stubs only the things a
// millisecond-scale test cannot have: the transport is a [MemoryBus] rather
// than Redis, and the database is a fake that models the RLS policies rather
// than a container. Both real versions are exercised in
// realtime_integration_test.go, behind the integration tag.
//
// The router is the real one. That matters more than it looks: it means every
// test below goes through api.NewRouter, the same requireAuth middleware, the
// same token verification and the same route table that production uses, so a
// test cannot pass because it mounted the handler somewhere friendlier.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/api"
	"github.com/AndyV99/collabboard/apps/api/internal/auth"
	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

const (
	testSecret   = "0123456789abcdef0123456789abcdef"
	testIssuerID = "collabboard-test"
	testAudience = "collabboard-api-test"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, nil))
}

func testIssuer(t *testing.T, ttl time.Duration) *auth.Issuer {
	t.Helper()

	issuer, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    []byte(testSecret),
		Issuer:    testIssuerID,
		Audience:  testAudience,
		AccessTTL: ttl,
	})
	if err != nil {
		t.Fatalf("building the issuer: %v", err)
	}

	return issuer
}

// stubAuthService satisfies api.AuthService so the real router will mount the
// authenticated tree. None of its methods is reachable from the routes under
// test, and each one says so rather than returning a zero value that could make
// a broken test look like a passing one.
type stubAuthService struct{}

func (stubAuthService) Register(context.Context, auth.RegisterInput) (auth.RegisterResult, error) {
	panic("stubAuthService: Register is not reachable from the realtime routes")
}

func (stubAuthService) Login(context.Context, auth.LoginInput) (auth.LoginResult, error) {
	panic("stubAuthService: Login is not reachable from the realtime routes")
}

func (stubAuthService) Refresh(context.Context, string) (auth.LoginResult, error) {
	panic("stubAuthService: Refresh is not reachable from the realtime routes")
}

func (stubAuthService) Logout(context.Context, string) error {
	panic("stubAuthService: Logout is not reachable from the realtime routes")
}

func (stubAuthService) SwitchOrganization(context.Context, auth.Principal, uuid.UUID) (auth.LoginResult, error) {
	panic("stubAuthService: SwitchOrganization is not reachable from the realtime routes")
}

func (stubAuthService) Organizations(context.Context, auth.Principal) ([]auth.Organization, error) {
	panic("stubAuthService: Organizations is not reachable from the realtime routes")
}

// recordingStore is a fake database that models the row-level security policies
// rather than the rows.
//
// It answers exactly two questions, both the way Postgres would under
// ADR 0001's policies: a membership is visible only inside its own tenant's
// transaction, and a board is visible only inside its own tenant's. Everything
// else panics.
//
// It also records every tenant a transaction was opened for, which is the
// assertion bola_test.go leans on — a leak that filtered afterwards would still
// show up here.
type recordingStore struct {
	mu       sync.Mutex
	opened   []uuid.UUID
	members  map[uuid.UUID]map[uuid.UUID]struct{}
	boards   map[uuid.UUID]map[uuid.UUID]struct{}
	failWith error
}

func newRecordingStore() *recordingStore {
	return &recordingStore{
		members: map[uuid.UUID]map[uuid.UUID]struct{}{},
		boards:  map[uuid.UUID]map[uuid.UUID]struct{}{},
	}
}

func (r *recordingStore) seedMember(tenantID, userID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.members[tenantID] == nil {
		r.members[tenantID] = map[uuid.UUID]struct{}{}
	}

	r.members[tenantID][userID] = struct{}{}
}

func (r *recordingStore) seedBoard(tenantID, boardID uuid.UUID) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.boards[tenantID] == nil {
		r.boards[tenantID] = map[uuid.UUID]struct{}{}
	}

	r.boards[tenantID][boardID] = struct{}{}
}

func (r *recordingStore) WithTenant(ctx context.Context, tenantID uuid.UUID, fn store.TenantFunc) error {
	r.mu.Lock()
	r.opened = append(r.opened, tenantID)
	failure := r.failWith
	r.mu.Unlock()

	if failure != nil {
		return failure
	}

	return fn(ctx, recordingQuerier{store: r, tenantID: tenantID})
}

func (r *recordingStore) openedTenants() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]uuid.UUID(nil), r.opened...)
}

type recordingQuerier struct {
	store    *recordingStore
	tenantID uuid.UUID
}

func (q recordingQuerier) GetMembership(_ context.Context, userID uuid.UUID) (store.Membership, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	if _, ok := q.store.members[q.tenantID][userID]; !ok {
		return store.Membership{}, store.ErrNoRows
	}

	return store.Membership{ID: uuid.New(), TenantID: q.tenantID, UserID: userID, Role: "member"}, nil
}

func (q recordingQuerier) GetBoard(_ context.Context, boardID uuid.UUID) (store.Board, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	if _, ok := q.store.boards[q.tenantID][boardID]; !ok {
		return store.Board{}, store.ErrNoRows
	}

	return store.Board{ID: boardID, TenantID: q.tenantID, Name: "board"}, nil
}

func (q recordingQuerier) CreateOrganization(context.Context, store.CreateOrganizationParams) (store.Organization, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) CreateMembership(context.Context, store.CreateMembershipParams) (store.Membership, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListMembers(context.Context) ([]store.ListMembersRow, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListProjects(context.Context) ([]store.Project, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) CreateProject(context.Context, store.CreateProjectParams) (store.Project, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListColumnsByBoard(context.Context, uuid.UUID) ([]store.Column, error) {
	panic("recordingQuerier: not modelled")
}

func (q recordingQuerier) ListCardsByBoard(context.Context, uuid.UUID) ([]store.Card, error) {
	panic("recordingQuerier: not modelled")
}

// instance is one API process: a hub, its broker, and an HTTP server in front
// of the real router.
type instance struct {
	t      *testing.T
	hub    *Hub
	server *httptest.Server
	issuer *auth.Issuer
}

type instanceOptions struct {
	bus        *MemoryBus
	authorizer Authorizer
	issuer     *auth.Issuer

	sendBuffer          int
	pingInterval        time.Duration
	pongTimeout         time.Duration
	writeTimeout        time.Duration
	reauthorizeInterval time.Duration
	maxRooms            int
}

// newInstance builds one instance and registers its teardown.
func newInstance(t *testing.T, opts instanceOptions) *instance {
	t.Helper()

	gin.SetMode(gin.TestMode)

	if opts.bus == nil {
		opts.bus = NewMemoryBus()
	}

	if opts.issuer == nil {
		opts.issuer = testIssuer(t, 15*time.Minute)
	}

	hub, err := NewHub(HubConfig{
		Broker:                opts.bus.Broker(),
		Authorizer:            opts.authorizer,
		Logger:                discardLogger(),
		SendBuffer:            opts.sendBuffer,
		PingInterval:          opts.pingInterval,
		PongTimeout:           opts.pongTimeout,
		WriteTimeout:          opts.writeTimeout,
		ReauthorizeInterval:   opts.reauthorizeInterval,
		MaxRoomsPerConnection: opts.maxRooms,
		// httptest servers answer on 127.0.0.1:<random>, and a websocket
		// client sends no Origin unless told to, so this is only exercised by
		// the origin test.
		AllowedOrigins: []string{"*"},
	})
	if err != nil {
		t.Fatalf("building the hub: %v", err)
	}

	router := api.NewRouter(discardLogger(),
		api.HealthDeps{},
		api.AuthDeps{Service: stubAuthService{}, Verifier: opts.issuer},
		api.RealtimeDeps{Connect: hub.ConnectHandler(), PublishEvent: hub.PublishHandler()})

	server := httptest.NewServer(router)

	inst := &instance{t: t, hub: hub, server: server, issuer: opts.issuer}

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		// Errors are reported rather than ignored: a hub that cannot drain in
		// a test is a hub that will not drain in a deploy.
		if err := hub.Shutdown(shutdownCtx); err != nil {
			t.Errorf("shutting the hub down: %v", err)
		}

		server.Close()
	})

	return inst
}

func (i *instance) token(t *testing.T, principal auth.Principal) string {
	t.Helper()

	token, _, err := i.issuer.Issue(principal)
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	return token
}

// wsURL rewrites the httptest URL onto the ws scheme.
func (i *instance) wsURL() string {
	return "ws" + strings.TrimPrefix(i.server.URL, "http") + "/api/v1/ws"
}

// client is a connected WebSocket with a background reader, so a test can
// assert on what arrived without ever blocking the connection's control-frame
// handling.
type client struct {
	t      *testing.T
	ws     *websocket.Conn
	frames chan Frame

	closeMu   sync.Mutex
	closeErr  error
	closeSeen bool
	closed    chan struct{}
}

// dial opens a connection carrying the token in the Authorization header.
func (i *instance) dial(ctx context.Context, t *testing.T, token string) *client {
	t.Helper()

	return i.dialWith(ctx, t, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{Subprotocol},
	})
}

// dialSubprotocol opens a connection carrying the token the way a browser has
// to: in Sec-WebSocket-Protocol, because the browser API cannot set headers.
func (i *instance) dialSubprotocol(ctx context.Context, t *testing.T, token string) *client {
	t.Helper()

	return i.dialWith(ctx, t, &websocket.DialOptions{
		Subprotocols: []string{Subprotocol, "bearer.collabboard.v1." + token},
	})
}

// dialHeader opens a connection to an arbitrary URL with arbitrary headers, so
// the BOLA sweep can add the ones an API of this shape usually has.
func (i *instance) dialHeader(ctx context.Context, t *testing.T, url string, header http.Header) *client {
	t.Helper()

	return i.dialURL(ctx, t, url, &websocket.DialOptions{
		HTTPHeader:   header,
		Subprotocols: []string{Subprotocol},
	})
}

func (i *instance) dialWith(ctx context.Context, t *testing.T, opts *websocket.DialOptions) *client {
	t.Helper()

	return i.dialURL(ctx, t, i.wsURL(), opts)
}

func (i *instance) dialURL(ctx context.Context, t *testing.T, url string, opts *websocket.DialOptions) *client {
	t.Helper()

	ws, resp, err := websocket.Dial(ctx, url, opts)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		t.Fatalf("dialing the websocket: %v", err)
	}

	if got := ws.Subprotocol(); got != Subprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, Subprotocol)
	}

	c := &client{t: t, ws: ws, frames: make(chan Frame, 64), closed: make(chan struct{})}

	go c.read(ctx)

	t.Cleanup(func() { _ = ws.CloseNow() })

	return c
}

// dialRaw opens a connection and never reads from it.
//
// coder/websocket answers pings from inside its read loop, so a client that
// never reads never pongs. That is the closest thing to a peer that has
// vanished without a TCP FIN — a closed laptop lid — that a test on one machine
// can produce, and it is exactly what the reaper exists for.
func (i *instance) dialRaw(ctx context.Context, t *testing.T, token string) *websocket.Conn {
	t.Helper()

	ws, resp, err := websocket.Dial(ctx, i.wsURL(), &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{Subprotocol},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		t.Fatalf("dialing the websocket: %v", err)
	}

	t.Cleanup(func() { _ = ws.CloseNow() })

	return ws
}

func (c *client) read(ctx context.Context) {
	defer close(c.frames)

	for {
		_, data, err := c.ws.Read(ctx)
		if err != nil {
			c.closeMu.Lock()
			c.closeErr = err
			c.closeSeen = true
			c.closeMu.Unlock()
			close(c.closed)

			return
		}

		var frame Frame
		if err := json.Unmarshal(data, &frame); err != nil {
			return
		}

		select {
		case c.frames <- frame:
		case <-ctx.Done():
			return
		}
	}
}

// next waits for one frame.
func (c *client) next(timeout time.Duration) (Frame, bool) {
	c.t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case frame, ok := <-c.frames:
		return frame, ok
	case <-timer.C:
		return Frame{}, false
	}
}

// expect waits for a frame of a particular type, skipping others.
func (c *client) expect(kind string, timeout time.Duration) Frame {
	c.t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			c.t.Fatalf("timed out waiting for a %q frame", kind)
		}

		frame, ok := c.next(remaining)
		if !ok {
			c.t.Fatalf("timed out waiting for a %q frame", kind)
		}

		if frame.Type == kind {
			return frame
		}
	}
}

// expectNothing asserts silence, which is how "did not receive another tenant's
// event" is stated.
func (c *client) expectNothing(timeout time.Duration) {
	c.t.Helper()

	if frame, ok := c.next(timeout); ok {
		c.t.Fatalf("expected no frame, got %+v", frame)
	}
}

func (c *client) send(t *testing.T, ctx context.Context, frame clientFrame) {
	t.Helper()

	encoded, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("encoding a client frame: %v", err)
	}

	if err := c.ws.Write(ctx, websocket.MessageText, encoded); err != nil {
		t.Fatalf("writing a client frame: %v", err)
	}
}

// subscribe sends a subscribe and waits for the acknowledgement.
func (c *client) subscribe(t *testing.T, ctx context.Context, boardID uuid.UUID) {
	t.Helper()

	c.send(t, ctx, clientFrame{Type: clientSubscribe, BoardID: boardID.String()})

	frame := c.expect(FrameSubscribed, 5*time.Second)
	if frame.BoardID == nil || *frame.BoardID != boardID {
		t.Fatalf("subscribed to %v, want %s", frame.BoardID, boardID)
	}
}

// closeStatus waits for the connection to end and reports the close code.
func (c *client) closeStatus(timeout time.Duration) websocket.StatusCode {
	c.t.Helper()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-c.closed:
	case <-timer.C:
		c.t.Fatal("timed out waiting for the connection to close")
	}

	c.closeMu.Lock()
	defer c.closeMu.Unlock()

	return websocket.CloseStatus(c.closeErr)
}

// publishResult is what the publish helper reports. The body is read and the
// response closed inside the helper, so no test has to remember to.
type publishResult struct {
	StatusCode int
	Body       string
}

// publish posts an event through the real HTTP endpoint.
func (i *instance) publish(t *testing.T, ctx context.Context, token string, boardID uuid.UUID, kind string) publishResult {
	t.Helper()

	body := strings.NewReader(`{"type":"` + kind + `","payload":{"card_id":"c1"}}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		i.server.URL+"/api/v1/boards/"+boardID.String()+"/events", body)
	if err != nil {
		t.Fatalf("building the publish request: %v", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := i.server.Client().Do(req)
	if err != nil {
		t.Fatalf("publishing: %v", err)
	}

	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			t.Errorf("closing the publish response: %v", cerr)
		}
	}()

	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the publish response: %v", err)
	}

	return publishResult{StatusCode: resp.StatusCode, Body: string(payload)}
}

// websocketDial is the raw dial, for tests that want the refusal rather than a
// connection.
func websocketDial(ctx context.Context, url, token string) (*websocket.Conn, *http.Response, error) {
	return websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": []string{"Bearer " + token}},
		Subprotocols: []string{Subprotocol},
	})
}

// eventually polls a condition, for the handful of assertions about hub state
// that are true only after a goroutine has finished reacting.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(2 * time.Millisecond)
	}

	t.Fatalf("timed out waiting for %s", what)
}

// allowAll is the authorizer for tests that are not about authorization.
type allowAll struct{}

func (allowAll) AuthorizeBoard(context.Context, auth.Principal, uuid.UUID) error { return nil }
func (allowAll) AuthorizeTenant(context.Context, auth.Principal) error           { return nil }

// scriptedAuthorizer answers from values a test can change mid-connection,
// which is how revocation is simulated without a database.
type scriptedAuthorizer struct {
	mu       sync.Mutex
	tenant   error
	board    error
	tenantN  int
	boardN   int
	boardFor map[uuid.UUID]error
}

func newScriptedAuthorizer() *scriptedAuthorizer {
	return &scriptedAuthorizer{boardFor: map[uuid.UUID]error{}}
}

func (a *scriptedAuthorizer) AuthorizeBoard(_ context.Context, _ auth.Principal, boardID uuid.UUID) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.boardN++

	if err, ok := a.boardFor[boardID]; ok {
		return err
	}

	return a.board
}

func (a *scriptedAuthorizer) AuthorizeTenant(context.Context, auth.Principal) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.tenantN++

	return a.tenant
}

func (a *scriptedAuthorizer) revokeTenant() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.tenant = ErrForbidden
}

func (a *scriptedAuthorizer) revokeBoard(boardID uuid.UUID) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.boardFor[boardID] = ErrForbidden
}

func (a *scriptedAuthorizer) failTenantWith(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.tenant = err
}

func (a *scriptedAuthorizer) counts() (tenant, board int) {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.tenantN, a.boardN
}

var errAuthorizerUnavailable = errors.New("authorizer: database unreachable")
