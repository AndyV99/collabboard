package auth_test

// The two queriers fakeStore hands out.
//
// Every method the auth flows do not use panics rather than returning a zero
// value. A fake that quietly answers a question it does not model is how a test
// comes to pass against behaviour nobody implemented.

import (
	"context"
	"crypto/sha256"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/AndyV99/collabboard/apps/api/internal/store"
)

// fakeIdentityQuerier implements store.IdentityQuerier.
type fakeIdentityQuerier struct{ store *fakeStore }

func (q fakeIdentityQuerier) FindUserByEmail(_ context.Context, email string) (store.IdentityUser, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	user, ok := q.store.usersByEmail[email]
	if !ok {
		return store.IdentityUser{}, store.ErrNotFound
	}

	return user, nil
}

func (q fakeIdentityQuerier) ResolveUserIDByEmail(_ context.Context, email string) (uuid.UUID, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	user, ok := q.store.usersByEmail[email]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}

	return user.ID, nil
}

func (q fakeIdentityQuerier) ListUserOrganizations(_ context.Context, userID uuid.UUID) ([]store.UserOrganization, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	return q.store.memberships[userID], nil
}

func (q fakeIdentityQuerier) CreateUser(_ context.Context, arg store.CreateUserParams) (store.CreatedUser, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	if _, taken := q.store.usersByEmail[arg.Email]; taken {
		// The same shape the real unique index produces, so
		// store.IsUniqueViolation answers the same way it does in production.
		return store.CreatedUser{}, &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"users_email_key\""}
	}

	user := store.IdentityUser{ID: uuid.New(), Email: arg.Email, DisplayName: arg.DisplayName}
	q.store.usersByEmail[arg.Email] = user
	q.store.usersByID[user.ID] = user

	return store.CreatedUser(user), nil
}

func (q fakeIdentityQuerier) PasswordParams(_ context.Context, userID uuid.UUID) (store.PasswordKDFParams, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	credential, ok := q.store.credentials[userID]
	if !ok {
		return store.PasswordKDFParams{}, store.ErrNotFound
	}

	return credential.params, nil
}

// VerifyPassword hashes the key it is given and compares, exactly as
// identity_verify_password does. Storing the digest rather than the key is what
// makes the "the stored value cannot be replayed" property testable here as
// well as against a real database.
func (q fakeIdentityQuerier) VerifyPassword(_ context.Context, arg store.VerifyPasswordParams) (uuid.UUID, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	credential, ok := q.store.credentials[arg.UserID]
	if !ok {
		return uuid.Nil, store.ErrNotFound
	}

	if sha256.Sum256(arg.Key) != credential.verifier {
		return uuid.Nil, store.ErrNotFound
	}

	return arg.UserID, nil
}

func (q fakeIdentityQuerier) CreatePassword(_ context.Context, arg store.CreatePasswordParams) (uuid.UUID, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	if _, exists := q.store.credentials[arg.UserID]; exists {
		return uuid.Nil, &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"user_credentials_pkey\""}
	}

	q.store.credentials[arg.UserID] = fakeCredential{
		params: store.PasswordKDFParams{
			Salt:        arg.Salt,
			MemoryKib:   arg.MemoryKib,
			Iterations:  arg.Iterations,
			Parallelism: arg.Parallelism,
			KeyLength:   arg.KeyLength,
		},
		verifier: sha256.Sum256(arg.Key),
	}

	return arg.UserID, nil
}

// fakeTenantQuerier implements store.Querier.
//
// The embedded interface is nil and is the "not modelled" case: the auth flows
// use two of the querier's methods, and the rest belong to the board and card
// surface. Calling one panics with a nil dereference naming the method, which is
// as loud as the hand-written panics it replaced and — unlike them — does not
// have to be extended every time query.sql grows a query the auth package will
// never call.
type fakeTenantQuerier struct {
	store.Querier

	store    *fakeStore
	tenantID uuid.UUID
}

func (q fakeTenantQuerier) CreateOrganization(_ context.Context, arg store.CreateOrganizationParams) (store.Organization, error) {
	// The id is the transaction's tenant, exactly as the real INSERT takes it
	// from current_tenant_id().
	return store.Organization{ID: q.tenantID, Name: arg.Name, Slug: arg.Slug}, nil
}

// CreateMembership models the unique index on (tenant_id, user_id) as well as
// the insert.
//
// The duplicate case is not incidental here: AddMember relies on the index
// rather than on a prior SELECT to refuse a second membership, so a fake that
// happily appended a second row would let the "duplicate is a clean 409" test
// pass against code that never checked.
func (q fakeTenantQuerier) CreateMembership(_ context.Context, arg store.CreateMembershipParams) (store.Membership, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	for _, existing := range q.store.memberships[arg.UserID] {
		if existing.OrganizationID == q.tenantID {
			return store.Membership{}, &pgconn.PgError{
				Code:    "23505",
				Message: "duplicate key value violates unique constraint \"memberships_tenant_id_user_id_key\"",
			}
		}
	}

	q.store.memberships[arg.UserID] = append(q.store.memberships[arg.UserID], store.UserOrganization{
		OrganizationID: q.tenantID,
		Name:           "org " + q.tenantID.String()[:8],
		Slug:           "org-" + q.tenantID.String()[:8],
		Role:           arg.Role,
	})

	return store.Membership{
		ID: uuid.New(), TenantID: q.tenantID, UserID: arg.UserID, Role: arg.Role, CreatedAt: time.Now(),
	}, nil
}

// GetMembership answers only for the transaction's own tenant, exactly as the
// policy on memberships does — which is what makes "a caller holding a token
// for an organization they were removed from is refused" testable here.
func (q fakeTenantQuerier) GetMembership(_ context.Context, userID uuid.UUID) (store.Membership, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	for _, existing := range q.store.memberships[userID] {
		if existing.OrganizationID == q.tenantID {
			return store.Membership{
				ID: uuid.New(), TenantID: q.tenantID, UserID: userID, Role: existing.Role,
			}, nil
		}
	}

	return store.Membership{}, store.ErrNoRows
}
