package api

// The body limit, measured at the socket instead of at the handler.
//
// httptest.NewRecorder proves what status came back. It cannot prove the thing
// the issue is actually about, which is that the process never took the body off
// the wire — a middleware that read a gigabyte into memory and *then* answered
// 413 would pass every test in bodylimit_test.go.
//
// So these run against a real listener that counts the bytes read from every
// accepted connection. The assertion is on that count: a request that declares
// eight gigabytes must cost this process a few hundred bytes, and a client that
// streams without ever declaring a length must be cut off rather than absorbed.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

// socketDeadline bounds every raw exchange in this file. It is a backstop for a
// hang, not a timing assertion: a body limit that deadlocks the connection
// instead of refusing it is the failure mode worth catching, and without a
// deadline that failure would look like a test that never finishes.
const socketDeadline = 15 * time.Second

// countingListener counts every byte this process reads from the network.
type countingListener struct {
	net.Listener

	read *atomic.Int64
}

func (l *countingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}

	return &countingConn{Conn: conn, read: l.read}, nil
}

type countingConn struct {
	net.Conn

	read *atomic.Int64
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))

	return n, err
}

// countingServer is the real router on a real socket, with the byte counter in
// front of it.
type countingServer struct {
	*bodyLimitFixture

	server *httptest.Server
	read   *atomic.Int64
}

func newCountingServer(t *testing.T, limits BodyLimits) *countingServer {
	t.Helper()

	fixture := newBodyLimitFixture(t, limits)
	server := httptest.NewUnstartedServer(fixture.router)
	read := new(atomic.Int64)

	server.Listener = &countingListener{Listener: server.Listener, read: read}

	server.Start()
	t.Cleanup(server.Close)

	return &countingServer{bodyLimitFixture: fixture, server: server, read: read}
}

// dial opens a raw connection to the server, so that a test can send a request
// no net/http client would agree to send.
func (s *countingServer) dial(t *testing.T) net.Conn {
	t.Helper()

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp", s.server.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dialing: %v", err)
	}

	if err := conn.SetDeadline(time.Now().Add(socketDeadline)); err != nil {
		t.Fatalf("setting a deadline: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// rawResponse is one response read off a raw connection.
//
// A value rather than the *http.Response, so that the connection's body is
// consumed and closed here rather than by every caller — and so that the
// caller's assertions are about what came back, not about lifecycle.
type rawResponse struct {
	status string
	code   int
	body   []byte

	// closing is whether the server said this connection is finished, which is
	// the observable half of "an oversized body does not leave a half-read
	// connection behind".
	closing bool
}

// readResponse parses one response off a raw connection, body and all.
func readResponse(t *testing.T, conn net.Conn) rawResponse {
	t.Helper()

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("reading the response: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the response body: %v", err)
	}

	return rawResponse{
		status:  resp.Status,
		code:    resp.StatusCode,
		body:    body,
		closing: resp.Close || resp.Header.Get("Connection") == "close",
	}
}

// TestABodyThatDeclaresItselfOversizedIsRefusedUnread is the headline case from
// #50: an unauthenticated login with a body far larger than this process could
// hold.
//
// The declared length is eight gigabytes. The assertion is that answering it
// costs a few hundred bytes of reading — so the answer cannot have involved
// allocating the body, on any implementation, whatever the status code says.
func TestABodyThatDeclaresItselfOversizedIsRefusedUnread(t *testing.T) {
	t.Parallel()

	server := newCountingServer(t, BodyLimits{})
	conn := server.dial(t)

	const (
		declared = 8 << 30
		sent     = 4 << 10
	)

	request := fmt.Sprintf(
		"POST /api/v1/auth/login HTTP/1.1\r\nHost: collabboard.test\r\n"+
			"Content-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		declared, `{"email":"someone@example.com","password":"`+strings.Repeat("a", sent))

	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatalf("writing the request: %v", err)
	}

	resp := readResponse(t, conn)

	read := server.read.Load()
	t.Logf("declared %d bytes, sent %d, this process read %d, answer %s %s (closing: %t)",
		declared, sent, read, resp.status, resp.body, resp.closing)

	if resp.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %s, want 413", resp.status)
	}

	assertTooLargeBody(t, resp.body)

	// Headers plus whatever of the body happened to be sitting in the server's
	// read buffer already. Anything beyond this means the body was being
	// consumed rather than refused.
	if maximum := int64(sent + 4<<10); read > maximum {
		t.Errorf("read %d bytes answering a request it refused; want at most %d", read, maximum)
	}

	if got := server.service.logins.Load(); got != 0 {
		t.Errorf("the auth service saw %d logins; the refusal must precede authentication", got)
	}

	// The connection is not left half-read with gigabytes of unsent body still
	// to come: net/http says so on the way out, and the next request on this
	// connection would otherwise be parsed out of the previous one's body.
	if !resp.closing {
		t.Errorf("the connection was kept alive with %d bytes of undelivered body outstanding", declared-sent)
	}
}

// TestABodyWithNoContentLengthIsStillBounded is the case a header check alone
// would miss. A chunked request declares no length at all, so the only thing
// that can refuse it is a reader that counts.
func TestABodyWithNoContentLengthIsStillBounded(t *testing.T) {
	t.Parallel()

	server := newCountingServer(t, BodyLimits{})

	// Deliberately only a little over the limit, so the client can finish
	// sending it and the exchange is entirely deterministic. The unbounded
	// version is the next test.
	//
	// Well-formed JSON as far as it goes — an open string that runs past the
	// limit — so that the refusal is the limit's and not the decoder's opinion
	// of the first byte.
	body := strings.NewReader(
		`{"email":"` + strings.Repeat("a", fallbackMaxUnauthenticatedRequestBytes+4<<10) + `"}`)

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		server.server.URL+"/api/v1/auth/login", &unmeasuredReader{Reader: body})
	if err != nil {
		t.Fatalf("building the request: %v", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := server.server.Client().Do(req)
	if err != nil {
		t.Fatalf("posting: %v", err)
	}

	defer func() { _ = resp.Body.Close() }()

	t.Logf("chunked, %d bytes, Content-Length %q -> %s",
		body.Size(), req.Header.Get("Content-Length"), resp.Status)

	if req.ContentLength > 0 {
		t.Fatalf("the request declared a length of %d; this test is not testing what it claims", req.ContentLength)
	}

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %s, want 413: a body with no declared length is not being bounded", resp.Status)
	}

	if got := server.service.logins.Load(); got != 0 {
		t.Errorf("the auth service saw %d logins", got)
	}
}

// unmeasuredReader hides the concrete reader type from net/http, which would
// otherwise recognise a *strings.Reader and set Content-Length for us — turning
// the test into the one above.
type unmeasuredReader struct{ *strings.Reader }

// TestAnEndlessBodyIsCutOffRatherThanAbsorbed is the denial-of-service case in
// its most honest form: a client that declares nothing and simply keeps
// sending.
//
// Nothing about this request tells the server how big it is, so the limit is the
// only thing between it and the process's memory. The assertion is the byte
// count again: the client offers 64 MiB and this process must take a fraction of
// a megabyte of it before answering and hanging up.
func TestAnEndlessBodyIsCutOffRatherThanAbsorbed(t *testing.T) {
	t.Parallel()

	server := newCountingServer(t, BodyLimits{})
	conn := server.dial(t)

	const offered = 64 << 20

	header := "POST /api/v1/auth/login HTTP/1.1\r\nHost: collabboard.test\r\n" +
		"Content-Type: application/json\r\nTransfer-Encoding: chunked\r\n\r\n"
	if _, err := conn.Write([]byte(header)); err != nil {
		t.Fatalf("writing the request head: %v", err)
	}

	var (
		wrote atomic.Int64
		wg    sync.WaitGroup
	)

	wg.Add(1)

	go func() {
		defer wg.Done()

		// A JSON string that is opened and never closed, so the decoder can
		// never finish and stop reading of its own accord: what stops this is
		// the limit or nothing.
		chunk := `{"email":"someone@example.com","password":"` + strings.Repeat("a", 32<<10)

		for wrote.Load() < offered {
			framed := fmt.Sprintf("%x\r\n%s\r\n", len(chunk), chunk)

			n, err := conn.Write([]byte(framed))
			wrote.Add(int64(n))

			if err != nil {
				return
			}

			chunk = strings.Repeat("a", 32<<10)
		}
	}()

	resp := readResponse(t, conn)

	// Stopping the writer: the server has already answered, and closing the
	// connection is what a client would do next anyway.
	_ = conn.Close()

	wg.Wait()

	read := server.read.Load()
	t.Logf("offered %d bytes with no declared length, wrote %d, this process read %d, answer %s %s",
		offered, wrote.Load(), read, resp.status, resp.body)

	if resp.code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %s, want 413", resp.status)
	}

	assertTooLargeBody(t, resp.body)

	// The limit itself, plus what net/http discards looking for the end of the
	// request before it gives up and closes the connection: up to 256 KiB when
	// the response header is flushed and up to 256 KiB again when the body is
	// closed. Everything past that would be this process absorbing the flood.
	if maximum := int64(fallbackMaxUnauthenticatedRequestBytes + 1<<20); read > maximum {
		t.Errorf("read %d bytes of an endless body; want at most %d", read, maximum)
	}

	if wrote.Load() >= offered {
		t.Errorf("the client got all %d bytes onto the wire; the flood was absorbed, not cut off", offered)
	}

	if got := server.service.logins.Load(); got != 0 {
		t.Errorf("the auth service saw %d logins", got)
	}
}

// TestTheWebSocketUpgradeIsNotCappedIntoFailure is the other half of the
// requirement: an upgrade is not a request body, and a limit that refused it
// would have taken the realtime surface down to fix an unrelated problem.
//
// The limit here is one byte — smaller than any HTTP request can be. If the
// upgrade were being measured as a body at all, or if wrapping the body
// interfered with hijacking the connection, nothing below would work. The POST
// at the end is the control: on this same router, one byte is genuinely the
// limit.
func TestTheWebSocketUpgradeIsNotCappedIntoFailure(t *testing.T) {
	t.Parallel()

	gin.SetMode(gin.TestMode)

	const subprotocol = "collabboard.v1"

	issuer := testIssuer(t)

	router := NewRouter(discardLogger(), BodyLimits{Default: 1}, nil,
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}},
		AuthDeps{Service: &countingAuthService{}, Verifier: issuer, Store: newCRUDStore()},
		RealtimeDeps{Connect: echoUpgradeHandler(t, subprotocol)})

	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	token, _, err := issuer.Issue(auth.Principal{
		UserID: uuid.New(), TenantID: uuid.New(), Role: "owner", SessionID: uuid.New(),
	})
	if err != nil {
		t.Fatalf("issuing a token: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), socketDeadline)
	defer cancel()

	// The browser's credential transport, so this covers the upgrade exactly as
	// the web app performs it.
	ws, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/v1/ws",
		&websocket.DialOptions{
			Subprotocols: []string{subprotocol, bearerSubprotocolPrefix + token},
		})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}

	if err != nil {
		t.Fatalf("the upgrade failed under a one byte body limit: %v", err)
	}

	defer func() { _ = ws.CloseNow() }()

	if got := ws.Subprotocol(); got != subprotocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, subprotocol)
	}

	// A subscribe, and the event it earns. Frames are not request bodies either,
	// and both directions have to keep working after the handshake.
	if err := ws.Write(ctx, websocket.MessageText, []byte(`{"type":"subscribe","board_id":"a-board"}`)); err != nil {
		t.Fatalf("sending a subscribe frame: %v", err)
	}

	kind, frame, err := ws.Read(ctx)
	if err != nil {
		t.Fatalf("reading the event: %v", err)
	}

	t.Logf("upgraded, subscribed, and received a %v frame: %s", kind, frame)

	if want := `{"type":"card.created"}`; string(frame) != want {
		t.Errorf("frame = %s, want %s", frame, want)
	}

	// The control. Without this the test above would pass just as happily
	// against a router with no limit at all.
	post, err := http.NewRequestWithContext(ctx, http.MethodPost,
		server.URL+"/api/v1/auth/login", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("building the control request: %v", err)
	}

	post.Header.Set("Content-Type", "application/json")

	control, err := server.Client().Do(post)
	if err != nil {
		t.Fatalf("posting the control request: %v", err)
	}

	defer func() { _ = control.Body.Close() }()

	t.Logf("the control: a two byte POST on the same router -> %s", control.Status)

	if control.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("the control POST -> %s, want 413: the one byte limit was not in force", control.Status)
	}
}

// echoUpgradeHandler is the smallest possible stand-in for the realtime hub: it
// upgrades, waits for one frame, and answers with one.
//
// internal/realtime cannot be imported here — it imports this package — so the
// full connect/subscribe/receive path against the real hub is asserted in
// internal/realtime's own suite, which builds its router through NewRouter and
// therefore runs through this middleware too. What this proves is the part that
// belongs to this package: the middleware does not break an upgrade.
func echoUpgradeHandler(t *testing.T, subprotocol string) gin.HandlerFunc {
	t.Helper()

	return func(c *gin.Context) {
		ws, err := websocket.Accept(c.Writer, c.Request, &websocket.AcceptOptions{
			Subprotocols:   []string{subprotocol},
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			t.Errorf("accepting the upgrade: %v", err)

			return
		}

		defer func() { _ = ws.CloseNow() }()

		if _, _, err := ws.Read(c.Request.Context()); err != nil {
			t.Errorf("reading the subscribe frame: %v", err)

			return
		}

		if err := ws.Write(c.Request.Context(), websocket.MessageText, []byte(`{"type":"card.created"}`)); err != nil {
			t.Errorf("writing the event: %v", err)
		}
	}
}
