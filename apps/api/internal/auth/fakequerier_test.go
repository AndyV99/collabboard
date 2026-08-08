package auth_test

// The two queriers fakeStore hands out.
//
// Every method the auth flows do not use panics rather than returning a zero
// value. A fake that quietly answers a question it does not model is how a test
// comes to pass against behaviour nobody implemented.

import (
	"context"
	"crypto/sha256"

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
type fakeTenantQuerier struct {
	store    *fakeStore
	tenantID uuid.UUID
}

func (q fakeTenantQuerier) CreateOrganization(_ context.Context, arg store.CreateOrganizationParams) (store.Organization, error) {
	// The id is the transaction's tenant, exactly as the real INSERT takes it
	// from current_tenant_id().
	return store.Organization{ID: q.tenantID, Name: arg.Name, Slug: arg.Slug}, nil
}

func (q fakeTenantQuerier) CreateMembership(_ context.Context, arg store.CreateMembershipParams) (store.Membership, error) {
	q.store.mu.Lock()
	defer q.store.mu.Unlock()

	q.store.memberships[arg.UserID] = append(q.store.memberships[arg.UserID], store.UserOrganization{
		OrganizationID: q.tenantID,
		Name:           "org " + q.tenantID.String()[:8],
		Slug:           "org-" + q.tenantID.String()[:8],
		Role:           arg.Role,
	})

	return store.Membership{TenantID: q.tenantID, UserID: arg.UserID, Role: arg.Role}, nil
}

func (q fakeTenantQuerier) ListMembers(context.Context) ([]store.ListMembersRow, error) {
	panic("fakeTenantQuerier: ListMembers is not modelled; the auth flows do not use it")
}

func (q fakeTenantQuerier) ListProjects(context.Context) ([]store.Project, error) {
	panic("fakeTenantQuerier: ListProjects is not modelled; the auth flows do not use it")
}

func (q fakeTenantQuerier) CreateProject(context.Context, store.CreateProjectParams) (store.Project, error) {
	panic("fakeTenantQuerier: CreateProject is not modelled; the auth flows do not use it")
}

func (q fakeTenantQuerier) GetBoard(context.Context, uuid.UUID) (store.Board, error) {
	panic("fakeTenantQuerier: GetBoard is not modelled; the auth flows do not use it")
}

func (q fakeTenantQuerier) ListColumnsByBoard(context.Context, uuid.UUID) ([]store.Column, error) {
	panic("fakeTenantQuerier: ListColumnsByBoard is not modelled; the auth flows do not use it")
}

func (q fakeTenantQuerier) ListCardsByBoard(context.Context, uuid.UUID) ([]store.Card, error) {
	panic("fakeTenantQuerier: ListCardsByBoard is not modelled; the auth flows do not use it")
}
