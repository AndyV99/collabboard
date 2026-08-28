package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// clientIPSeenBy builds a router with the given trusted-proxy list, sends one
// request through it carrying the given headers from the given peer, and
// reports what ClientIP() resolved to.
//
// It drives a real engine rather than calling gin's helpers directly, because
// the thing under test is the wiring in NewRouter -- SetTrustedProxies and
// RemoteIPHeaders together. A unit test of gin's own ClientIP would pass
// whether or not NewRouter had ever called either.
func clientIPSeenBy(t *testing.T, trusted []string, peer string, headers map[string]string) string {
	t.Helper()

	router := NewRouter(discardLogger(), BodyLimits{}, trusted,
		HealthDeps{Postgres: stubPinger{}, Redis: stubPinger{}}, AuthDeps{}, RealtimeDeps{})

	var seen string

	router.GET("/whoami", func(c *gin.Context) {
		seen = c.ClientIP()
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/whoami", nil)
	req.RemoteAddr = peer

	for name, value := range headers {
		req.Header.Set(name, value)
	}

	router.ServeHTTP(httptest.NewRecorder(), req)

	return seen
}

// The default, which is the behaviour before this setting existed and the right
// one for a service with nothing in front of it. A forged header from an
// untrusted peer must change nothing -- if it did, one header per attempt would
// make every login look like a new client and the per-address budget would be
// free to bypass.
func TestForgedForwardedHeaderIsIgnoredWithNoTrustedProxies(t *testing.T) {
	got := clientIPSeenBy(t, nil, "203.0.113.9:54321", map[string]string{
		forwardedForHeader: "198.51.100.7",
		"X-Real-IP":        "198.51.100.8",
	})

	if got != "203.0.113.9" {
		t.Errorf("ClientIP() = %q, want the peer address 203.0.113.9 -- a forged header from an untrusted peer must not be believed", got)
	}
}

// The same forgery, from a peer that IS in the trusted list. This is the case
// that makes the setting worth having: behind a load balancer, the peer is
// always the load balancer, so without this every request shares one bucket.
//
// The header here is what an ALB actually produces: it *appends* the address it
// received the connection from, so a client's own forged entry ends up to the
// left of the real one. gin walks the list right to left and stops at the first
// untrusted address, which is why the forgery does not win.
func TestForwardedHeaderIsBelievedFromATrustedProxy(t *testing.T) {
	got := clientIPSeenBy(t, []string{"10.0.0.0/20"}, "10.0.1.55:41000", map[string]string{
		forwardedForHeader: "198.51.100.7, 203.0.113.9",
	})

	if got != "203.0.113.9" {
		t.Errorf("ClientIP() = %q, want 203.0.113.9 -- the rightmost untrusted entry is the address the load balancer observed", got)
	}
}

// A peer outside the trusted range must not be believed even when the list is
// populated. Trusting a range means trusting that range, not "trusting
// forwarding headers in general".
func TestForgedForwardedHeaderIsIgnoredFromAnUntrustedPeer(t *testing.T) {
	got := clientIPSeenBy(t, []string{"10.0.0.0/20"}, "203.0.113.50:41000", map[string]string{
		forwardedForHeader: "198.51.100.7",
	})

	if got != "203.0.113.50" {
		t.Errorf("ClientIP() = %q, want the peer address 203.0.113.50 -- 203.0.113.50 is not in 10.0.0.0/20", got)
	}
}

// The header decision, asserted rather than left in a comment.
//
// gin's default RemoteIPHeaders is {"X-Forwarded-For", "X-Real-IP"} and it falls
// through to the second when the first is absent or unparseable. An ALB appends
// to X-Forwarded-For and passes X-Real-IP through untouched, so with the default
// list a client could send a deliberately malformed X-Forwarded-For alongside a
// chosen X-Real-IP and pick its own identity from a trusted peer.
//
// "not-an-address" is the realistic shape: gin's validateHeader breaks out of
// its right-to-left walk on the first unparseable entry and reports the header
// invalid, which is precisely what triggers the fallback.
func TestXRealIPIsNotConsultedEvenFromATrustedProxy(t *testing.T) {
	got := clientIPSeenBy(t, []string{"10.0.0.0/20"}, "10.0.1.55:41000", map[string]string{
		forwardedForHeader: "not-an-address",
		"X-Real-IP":        "198.51.100.7",
	})

	if got == "198.51.100.7" {
		t.Fatal("ClientIP() came from X-Real-IP: the load balancer does not write that header and passes a client's own through, so believing it hands the client its own identity")
	}

	if got != "10.0.1.55" {
		t.Errorf("ClientIP() = %q, want the peer address 10.0.1.55 -- an unusable X-Forwarded-For must fall back to the peer, not to another header", got)
	}
}
