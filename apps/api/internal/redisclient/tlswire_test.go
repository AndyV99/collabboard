package redisclient_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/AndyV99/collabboard/apps/api/internal/config"
	"github.com/AndyV99/collabboard/apps/api/internal/redisclient"
)

// These two tests exist because asserting that a struct field is non-nil does
// not prove that anything is encrypted. They put a real TLS listener in front
// of a socket and check what this package's Options actually put on the wire.
//
// What they prove: with the setting on, the client opens a TLS handshake and
// verifies the server's certificate against the system trust store; with it
// off, the client speaks plaintext. What they do not prove: interoperability
// with a real ElastiCache replication group, which no test in this repository
// can reach. See the package tests in redisclient_test.go for the settings-level
// assertions.

// handshake is what the fake server observed from one connection.
type handshake struct {
	sni string
	err error
}

// newTLSServer starts a TLS listener on localhost with a self-signed
// certificate and reports what each client connection did.
//
// The certificate is generated per test and trusted by nobody, which is the
// point: a client that verifies certificates must reject it, and that rejection
// is the evidence that verification is switched on.
func newTLSServer(t *testing.T) (port int, observed <-chan handshake) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}

	// Bound to the loopback address explicitly. The client dials this address
	// while being configured with a hostname, so the binding must not depend on
	// how localhost happens to resolve on this machine.
	//
	// context.Background rather than t.Context: the latter is cancelled before
	// t.Cleanup runs, which is where the listener is closed.
	var lc net.ListenConfig

	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	results := make(chan handshake, 8)

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}

			go func() {
				defer func() { _ = conn.Close() }()

				var sni string

				tlsConn := tls.Server(conn, &tls.Config{
					MinVersion: tls.VersionTLS12,
					GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
						sni = hello.ServerName

						return &cert, nil
					},
				})

				_ = tlsConn.SetDeadline(time.Now().Add(5 * time.Second))
				herr := tlsConn.HandshakeContext(context.Background())

				// Non-blocking so a connection the test never reads — the
				// client's pool may open more than one — cannot leave this
				// goroutine parked on a send after the test returns. The
				// buffer is sized well above the number of dials either test
				// provokes, so a drop here means something unexpected
				// happened rather than the test racing itself.
				select {
				case results <- handshake{sni: sni, err: herr}:
				default:
				}
			}()
		}
	}()

	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("splitting %q: %v", ln.Addr(), err)
	}

	bound, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}

	return bound, results
}

// serverHost is the name the client is configured with and the name the
// certificate is issued for.
//
// Deliberately a hostname rather than the listener's resolved 127.0.0.1.
// crypto/tls omits SNI entirely for an IP literal and requires an IP SAN to
// verify one, so configuring REDIS_HOST as an address instead of the
// ElastiCache endpoint's DNS name would fail verification — which the earlier
// draft of this test demonstrated by accident.
const serverHost = "localhost"

// clientFor builds a client from this package's Options, with retries off so a
// single dial produces a single observable handshake.
//
// The dial address is overridden to the loopback *address* while the
// configuration keeps the loopback *name*. That divergence is what makes the
// SNI assertion mean anything: crypto/tls fills in an empty ServerName from the
// address being dialled (see tls.Dial), so if the configured host and the
// dialled host are the same string, a client that never sets ServerName is
// indistinguishable from one that derives it correctly. Splitting them is also
// the real deployment shape — a name resolved elsewhere, or a proxy in front.
func clientFor(t *testing.T, port int, tlsEnabled bool) *redis.Client {
	t.Helper()

	opts := redisclient.Options(config.RedisConfig{
		Host:       serverHost,
		Port:       port,
		TLSEnabled: tlsEnabled,
	})
	opts.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	opts.MaxRetries = -1
	opts.DialTimeout = 5 * time.Second

	client := redis.NewClient(opts)

	t.Cleanup(func() { _ = client.Close() })

	return client
}

// With TLS on, the client must actually negotiate TLS, send the configured host
// as SNI, and refuse a certificate it cannot chain to a trusted root. The Ping
// is expected to fail — failing for the right reason is the assertion.
func TestEnabledClientNegotiatesTLSAndVerifies(t *testing.T) {
	port, observed := newTLSServer(t)
	client := clientFor(t, port, true)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := client.Ping(ctx).Err()
	if err == nil {
		t.Fatal("Ping succeeded against an untrusted self-signed certificate; verification is not happening")
	}

	// The specific failure proves verification ran, rather than the connection
	// falling over for some unrelated reason.
	//
	// CertificateVerificationError is the wrapper crypto/tls puts around every
	// verification failure, so this does not pin the platform verifier's error
	// type — x509.UnknownAuthorityError is a Linux shape, and darwin, Windows
	// and a root-less container each produce something different for the same
	// rejection.
	var verifyErr *tls.CertificateVerificationError
	if !errors.As(err, &verifyErr) {
		t.Fatalf("Ping failed with %v (%T), want a certificate verification error", err, err)
	}

	select {
	case got := <-observed:
		// A ClientHello reached the server, so TLS was genuinely spoken rather
		// than the client failing before it opened a handshake.
		//
		// This is also the assertion that fails if ServerName stops being
		// derived from the configured host: the dial address is an IP literal,
		// and crypto/tls sends no SNI at all for one, so the fallback produces
		// an empty string here rather than the name.
		if got.sni != serverHost {
			t.Errorf("server saw SNI %q, want %q — ServerName is not derived from the configured host",
				got.sni, serverHost)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server observed no TLS handshake at all")
	}
}

// With TLS off, the client must speak plaintext — this is what keeps
// `docker compose up` working. A TLS server sees the plaintext RESP bytes as a
// malformed record, which is the inverse evidence.
func TestDisabledClientSpeaksPlaintext(t *testing.T) {
	port, observed := newTLSServer(t)
	client := clientFor(t, port, false)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = client.Ping(ctx).Err()

	select {
	case got := <-observed:
		if got.err == nil {
			t.Fatal("a TLS handshake completed with TLS disabled")
		}

		var recordErr tls.RecordHeaderError
		if !errors.As(got.err, &recordErr) {
			t.Fatalf("server handshake failed with %v (%T), want a record-header error "+
				"showing the client sent plaintext", got.err, got.err)
		}

		if got.sni != "" {
			t.Errorf("server saw SNI %q with TLS disabled", got.sni)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server observed no connection attempt")
	}
}
