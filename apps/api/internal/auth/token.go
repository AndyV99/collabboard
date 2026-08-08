package auth

// Access tokens.
//
// Short-lived, signed, and carrying the tenant. The tenant claim is the reason
// this file matters more than its length suggests: it is what the HTTP
// middleware hands to internal/store, and store.WithTenant does not check it.
// ADR 0001 is explicit that the isolation layer is isolation and not
// authorization — RLS serves whatever tenant it is given. So the guarantee that
// a request cannot name someone else's tenant is entirely the guarantee that
// this claim came from a token this service signed, and that the value in it
// was derived from a membership at issue time.
//
// Which is why nothing in this package reads a tenant from a header, a path
// segment or a body. The only writer of the claim is [Issuer.Issue], and the
// only caller of that is a flow that has just checked a membership.

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// Errors returned when a presented token is not acceptable.
//
// They are distinguished here so the middleware can log *why* a token was
// rejected — expired and tampered are very different signals — while the
// response body says the same thing for all of them.
var (
	// ErrTokenMalformed means the string was not a JWT this service could parse.
	ErrTokenMalformed = errors.New("auth: token is malformed")

	// ErrTokenExpired means the signature was good and the token is past its
	// exp. Separated from ErrTokenInvalid because it is the *expected* failure
	// — clients hit it every access-token lifetime and refresh — and lumping it
	// in with signature failures would bury the ones worth alerting on.
	ErrTokenExpired = errors.New("auth: token has expired")

	// ErrTokenInvalid covers a bad signature, an unexpected algorithm, a wrong
	// issuer or audience, and claims that do not parse. All of them mean the
	// token was not issued by this service for this service.
	ErrTokenInvalid = errors.New("auth: token is invalid")
)

// signingMethod is the only algorithm this service will accept.
//
// Pinned, and passed to the parser as an allow-list rather than inferred from
// the token header. A parser that trusts the header is the "alg: none" and
// RS256→HS256 confusion class of bug: the attacker picks the algorithm and the
// verifier obliges. HS256 rather than an asymmetric method because there is one
// service, it both issues and verifies, and a shared secret it never publishes
// is simpler to rotate than a key pair nothing else consumes.
var signingMethod = jwt.SigningMethodHS256

// minSecretLength is the floor for the signing key, in bytes. HS256 is
// HMAC-SHA256, whose security is capped by the key's entropy; a short secret is
// a token forgery away from being brute-forced offline by anyone holding one
// valid token.
const minSecretLength = 32

// Principal is who a request is, and which tenant it acts in.
//
// TenantID is the field the whole isolation model leans on. It is never
// populated from request data — see the file comment.
type Principal struct {
	UserID    uuid.UUID
	TenantID  uuid.UUID
	Role      string
	SessionID uuid.UUID

	// ExpiresAt is the access token's expiry, carried so a handler can report
	// it without re-parsing the token.
	ExpiresAt time.Time
}

// Claims is the token payload: the registered claims plus the three this
// service adds.
//
// The tenant travels as "org" rather than as a second subject, because "sub"
// identifies the human and the same human legitimately holds tokens for several
// organizations at once.
type Claims struct {
	jwt.RegisteredClaims

	OrganizationID string `json:"org"`
	Role           string `json:"role"`
	SessionID      string `json:"sid"`
}

// TokenConfig configures issuing and verification.
type TokenConfig struct {
	// Secret is the HMAC key. At least minSecretLength bytes.
	Secret []byte

	// Issuer and Audience are asserted on verification, not merely stamped on
	// issue. Without that a token minted by some other service sharing the
	// secret — a future worker, a staging environment pointed at the same
	// secret store — would verify here.
	Issuer   string
	Audience string

	// AccessTTL is how long an access token is good for. Short, because it is
	// the window in which a revoked session or a removed membership still
	// works: nothing checks the database on a normal request.
	AccessTTL time.Duration

	// Leeway absorbs clock skew between the issuing and verifying processes.
	// Deliberately small: it extends the life of every expired token by
	// exactly this much.
	Leeway time.Duration
}

// Validate reports whether the configuration can be used to issue tokens.
func (c TokenConfig) Validate() error {
	switch {
	case len(c.Secret) < minSecretLength:
		return fmt.Errorf("auth: jwt secret is %d bytes, minimum %d", len(c.Secret), minSecretLength)
	case c.Issuer == "":
		return errors.New("auth: jwt issuer is required")
	case c.Audience == "":
		return errors.New("auth: jwt audience is required")
	case c.AccessTTL <= 0:
		return errors.New("auth: access token ttl must be positive")
	default:
		return nil
	}
}

// Issuer mints and verifies access tokens.
type Issuer struct {
	cfg    TokenConfig
	parser *jwt.Parser

	// now is time.Now, replaceable in tests. Token expiry is one of the few
	// behaviours where waiting for real time to pass is the only alternative.
	now func() time.Time
}

// NewIssuer returns an Issuer, or an error if the configuration would produce
// tokens this service should not accept.
func NewIssuer(cfg TokenConfig) (*Issuer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &Issuer{
		cfg: cfg,
		parser: jwt.NewParser(
			// The allow-list. Everything else in this call is a claim check;
			// this one is the signature check.
			jwt.WithValidMethods([]string{signingMethod.Alg()}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			// A token with no exp would otherwise validate forever.
			jwt.WithExpirationRequired(),
			jwt.WithLeeway(cfg.Leeway),
		),
		now: time.Now,
	}, nil
}

// AccessTTL reports the configured access-token lifetime, so a handler can put
// expires_in in a response without owning the number.
func (i *Issuer) AccessTTL() time.Duration { return i.cfg.AccessTTL }

// Issue mints an access token for p.
//
// The returned expiry is the one stamped into the token, not a recomputation:
// a client that trusts expires_in and a server that trusts exp must agree.
func (i *Issuer) Issue(p Principal) (token string, expiresAt time.Time, err error) {
	if p.UserID == uuid.Nil {
		return "", time.Time{}, errors.New("auth: cannot issue a token without a subject")
	}

	if p.TenantID == uuid.Nil {
		// Refused rather than issued with an empty org. uuid.Nil is a
		// syntactically valid tenant that matches no organization, so a token
		// carrying it would authenticate fine and then quietly return empty
		// results from every tenant-scoped query — "no data" where the truth is
		// "no tenant". Same reasoning as store.ErrNoTenant.
		return "", time.Time{}, errors.New("auth: cannot issue a token without a tenant")
	}

	issued := i.now()
	expires := issued.Add(i.cfg.AccessTTL)

	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   p.UserID.String(),
			Issuer:    i.cfg.Issuer,
			Audience:  jwt.ClaimStrings{i.cfg.Audience},
			IssuedAt:  jwt.NewNumericDate(issued),
			NotBefore: jwt.NewNumericDate(issued),
			ExpiresAt: jwt.NewNumericDate(expires),
			ID:        uuid.NewString(),
		},
		OrganizationID: p.TenantID.String(),
		Role:           p.Role,
		SessionID:      p.SessionID.String(),
	}

	signed, err := jwt.NewWithClaims(signingMethod, claims).SignedString(i.cfg.Secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("signing access token: %w", err)
	}

	return signed, expires, nil
}

// Verify parses and validates a token, returning the principal it describes.
//
// Every failure path returns one of the three sentinels and nothing derived
// from the token itself, so a caller cannot accidentally echo attacker-supplied
// content back in an error.
func (i *Issuer) Verify(token string) (Principal, error) {
	var claims Claims

	if _, err := i.parser.ParseWithClaims(token, &claims, func(*jwt.Token) (any, error) {
		return i.cfg.Secret, nil
	}); err != nil {
		return Principal{}, classifyTokenError(err)
	}

	userID, err := uuid.Parse(claims.Subject)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: subject is not a uuid", ErrTokenInvalid)
	}

	tenantID, err := uuid.Parse(claims.OrganizationID)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: org claim is not a uuid", ErrTokenInvalid)
	}

	if tenantID == uuid.Nil {
		return Principal{}, fmt.Errorf("%w: org claim is the zero uuid", ErrTokenInvalid)
	}

	sessionID, err := uuid.Parse(claims.SessionID)
	if err != nil {
		return Principal{}, fmt.Errorf("%w: sid claim is not a uuid", ErrTokenInvalid)
	}

	if claims.ExpiresAt == nil {
		// Belt and braces behind jwt.WithExpirationRequired, because the
		// principal carries the expiry and a nil dereference here would be a
		// panic on attacker-controlled input.
		return Principal{}, fmt.Errorf("%w: no expiry", ErrTokenInvalid)
	}

	return Principal{
		UserID:    userID,
		TenantID:  tenantID,
		Role:      claims.Role,
		SessionID: sessionID,
		ExpiresAt: claims.ExpiresAt.Time,
	}, nil
}

// classifyTokenError maps the library's error set onto this package's three
// sentinels. Expiry is checked first because a token can be both expired and
// otherwise fine, and "expired" is the more useful thing to log.
func classifyTokenError(err error) error {
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return ErrTokenExpired
	case errors.Is(err, jwt.ErrTokenMalformed):
		return ErrTokenMalformed
	default:
		return ErrTokenInvalid
	}
}
