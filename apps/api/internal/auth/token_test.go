package auth_test

// Access tokens: what is accepted, and — mostly — what is not.
//
// The rejection cases are the point. An issuer that mints a valid token is easy
// to write and easy to test; a verifier that refuses a token signed with a
// different key, or with "alg": "none", or by a different service sharing the
// secret, is the part that decides whether the tenant claim means anything.

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/AndyV99/collabboard/apps/api/internal/auth"
)

const (
	testSecret      = "0123456789abcdef0123456789abcdef"
	otherSecret     = "fedcba9876543210fedcba9876543210"
	testIssuerName  = "collabboard-test"
	testAudience    = "collabboard-api-test"
	testAccessTTL   = 15 * time.Minute
	testClockLeeway = 0
)

func newTestIssuer(t *testing.T, ttl time.Duration) *auth.Issuer {
	t.Helper()

	issuer, err := auth.NewIssuer(auth.TokenConfig{
		Secret:    []byte(testSecret),
		Issuer:    testIssuerName,
		Audience:  testAudience,
		AccessTTL: ttl,
		Leeway:    testClockLeeway,
	})
	if err != nil {
		t.Fatalf("building the issuer: %v", err)
	}

	return issuer
}

func testPrincipal() auth.Principal {
	return auth.Principal{
		UserID:    uuid.New(),
		TenantID:  uuid.New(),
		Role:      "owner",
		SessionID: uuid.New(),
	}
}

func TestAnIssuedTokenVerifiesBackToTheSamePrincipal(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t, testAccessTTL)
	want := testPrincipal()

	token, expires, err := issuer.Issue(want)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	got, err := issuer.Verify(token)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}

	t.Logf("round trip: user=%s tenant=%s role=%s session=%s exp=%s",
		got.UserID, got.TenantID, got.Role, got.SessionID, got.ExpiresAt.UTC().Format(time.RFC3339))

	if got.UserID != want.UserID || got.TenantID != want.TenantID ||
		got.Role != want.Role || got.SessionID != want.SessionID {
		t.Errorf("Verify = %+v, want %+v", got, want)
	}

	// The expiry a client is told and the expiry the token carries have to be
	// the same instant, or a client refreshes either too early or too late.
	if !got.ExpiresAt.Equal(expires.Truncate(time.Second)) {
		t.Errorf("token exp = %s, Issue reported %s", got.ExpiresAt, expires)
	}
}

func TestIssueRefusesAPrincipalWithNoTenant(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t, testAccessTTL)

	for _, tc := range []struct {
		name      string
		principal auth.Principal
	}{
		{name: "no subject", principal: auth.Principal{TenantID: uuid.New()}},
		{name: "no tenant", principal: auth.Principal{UserID: uuid.New()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, _, err := issuer.Issue(tc.principal)

			t.Logf("Issue(%s) -> %v", tc.name, err)

			// uuid.Nil is a syntactically valid tenant matching no
			// organization, so a token carrying it would authenticate and then
			// return empty results from every query — "no data" where the truth
			// is "no tenant".
			if err == nil {
				t.Error("Issue succeeded; a token with a zero tenant would authenticate and silently see nothing")
			}
		})
	}
}

// TestVerifyRejects is the table that matters. Each row is a token an attacker
// can actually produce.
func TestVerifyRejects(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t, testAccessTTL)
	principal := testPrincipal()

	valid, _, err := issuer.Issue(principal)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	for _, tc := range []struct {
		name  string
		token func(t *testing.T) string
		want  error
	}{
		{
			name:  "empty string",
			token: func(*testing.T) string { return "" },
			want:  auth.ErrTokenMalformed,
		},
		{
			name:  "not a jwt",
			token: func(*testing.T) string { return "hello" },
			want:  auth.ErrTokenMalformed,
		},
		{
			name: "signature tampered",
			token: func(t *testing.T) string {
				// One character of the signature changed. The header and
				// payload are untouched, so this is precisely "a valid token
				// whose signature no longer matches".
				return tamperSignature(t, valid)
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "payload tampered, signature left alone",
			token: func(t *testing.T) string {
				// The attack this whole file exists for: change the org claim
				// to some other tenant and present it.
				return tamperOrgClaim(t, valid, uuid.NewString())
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "signed with a different key",
			token: func(t *testing.T) string {
				return signWith(t, []byte(otherSecret), jwt.SigningMethodHS256, claimsFor(principal, testIssuerName, testAudience))
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "alg none",
			token: func(t *testing.T) string {
				// jwt.SigningMethodNone with the library's sentinel key. If the
				// parser inferred the algorithm from the header instead of
				// taking an allow-list, this would verify.
				token := jwt.NewWithClaims(jwt.SigningMethodNone, claimsFor(principal, testIssuerName, testAudience))

				signed, serr := token.SignedString(jwt.UnsafeAllowNoneSignatureType)
				if serr != nil {
					t.Fatalf("signing an alg=none token: %v", serr)
				}

				return signed
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "issued by a different service sharing the secret",
			token: func(t *testing.T) string {
				return signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claimsFor(principal, "some-other-service", testAudience))
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "issued for a different audience",
			token: func(t *testing.T) string {
				return signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claimsFor(principal, testIssuerName, "some-other-api"))
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "no expiry",
			token: func(t *testing.T) string {
				claims := claimsFor(principal, testIssuerName, testAudience)
				claims.ExpiresAt = nil

				return signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "org claim is not a uuid",
			token: func(t *testing.T) string {
				claims := claimsFor(principal, testIssuerName, testAudience)
				claims.OrganizationID = "not-a-uuid"

				return signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
			},
			want: auth.ErrTokenInvalid,
		},
		{
			name: "org claim is the zero uuid",
			token: func(t *testing.T) string {
				claims := claimsFor(principal, testIssuerName, testAudience)
				claims.OrganizationID = uuid.Nil.String()

				return signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claims)
			},
			want: auth.ErrTokenInvalid,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := issuer.Verify(tc.token(t))

			t.Logf("%s -> %v", tc.name, err)

			if !errors.Is(err, tc.want) {
				t.Errorf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestVerifyRejectsAnExpiredToken is separate because expiry is the one
// rejection clients hit routinely, and it has to be distinguishable in the logs
// from the ones that mean someone is attacking.
func TestVerifyRejectsAnExpiredToken(t *testing.T) {
	t.Parallel()

	issuer := newTestIssuer(t, testAccessTTL)

	// Signed with the real key and correctly formed in every other way — the
	// only thing wrong with it is the hour on the clock. Built by hand rather
	// than by sleeping past a short ttl, which would put a real delay in the
	// unit loop.
	claims := claimsFor(testPrincipal(), testIssuerName, testAudience)
	claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute))
	claims.IssuedAt = jwt.NewNumericDate(time.Now().Add(-time.Hour))
	claims.NotBefore = claims.IssuedAt

	_, err := issuer.Verify(signWith(t, []byte(testSecret), jwt.SigningMethodHS256, claims))

	t.Logf("expired token -> %v", err)

	if !errors.Is(err, auth.ErrTokenExpired) {
		t.Errorf("Verify = %v, want %v", err, auth.ErrTokenExpired)
	}
}

func TestNewIssuerRejectsAConfigurationThatWouldWeakenTokens(t *testing.T) {
	t.Parallel()

	base := auth.TokenConfig{
		Secret:    []byte(testSecret),
		Issuer:    testIssuerName,
		Audience:  testAudience,
		AccessTTL: testAccessTTL,
	}

	for _, tc := range []struct {
		name  string
		mutta func(*auth.TokenConfig)
	}{
		{name: "short secret", mutta: func(c *auth.TokenConfig) { c.Secret = []byte("too short") }},
		{name: "no secret", mutta: func(c *auth.TokenConfig) { c.Secret = nil }},
		{name: "no issuer", mutta: func(c *auth.TokenConfig) { c.Issuer = "" }},
		{name: "no audience", mutta: func(c *auth.TokenConfig) { c.Audience = "" }},
		{name: "zero ttl", mutta: func(c *auth.TokenConfig) { c.AccessTTL = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := base
			tc.mutta(&cfg)

			_, err := auth.NewIssuer(cfg)

			t.Logf("%s -> %v", tc.name, err)

			if err == nil {
				t.Error("NewIssuer accepted a configuration it should refuse")
			}
		})
	}
}

func claimsFor(p auth.Principal, issuer, audience string) auth.Claims {
	now := time.Now()

	return auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{audience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(testAccessTTL)),
			ID:        uuid.NewString(),
		},
		OrganizationID: p.TenantID.String(),
		Role:           p.Role,
		SessionID:      p.SessionID.String(),
	}
}

func signWith(t *testing.T, key []byte, method jwt.SigningMethod, claims auth.Claims) string {
	t.Helper()

	signed, err := jwt.NewWithClaims(method, claims).SignedString(key)
	if err != nil {
		t.Fatalf("signing: %v", err)
	}

	return signed
}

// tamperSignature changes one character in the middle of the signature.
//
// The middle rather than the end: base64url's final character of a 32-byte
// signature carries only two significant bits, so several spellings of it
// decode to the same bytes and "flip the last character" can leave the
// signature unchanged. That is a test that passes for the wrong reason waiting
// to happen.
func tamperSignature(t *testing.T, token string) string {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[2]) < 4 {
		t.Fatalf("token is not a signed jwt: %q", token)
	}

	sig := []byte(parts[2])

	mid := len(sig) / 2
	if sig[mid] == 'A' {
		sig[mid] = 'B'
	} else {
		sig[mid] = 'A'
	}

	return parts[0] + "." + parts[1] + "." + string(sig)
}

// tamperOrgClaim rewrites the org claim in the payload and leaves the signature
// as it was — the naive privilege escalation, and the one a verifier that only
// decoded would fall for.
func tamperOrgClaim(t *testing.T, token, organizationID string) string {
	t.Helper()

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d parts, want 3", len(parts))
	}

	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decoding payload: %v", err)
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshalling payload: %v", err)
	}

	payload["org"] = organizationID

	edited, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshalling payload: %v", err)
	}

	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(edited) + "." + parts[2]
}
