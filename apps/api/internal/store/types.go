package store

import "github.com/AndyV99/collabboard/apps/api/internal/store/internal/gen"

// The generated types, re-exported as aliases.
//
// The sqlc output lives in internal/store/internal/gen so that no package
// outside internal/store can import it and build a querier against the pool.
// That restriction is on imports, not on values: callers still receive these
// types from a [TenantFunc] and need to be able to name them in a variable, a
// struct field or a function signature. Aliases give them the name without the
// import — the type is identical, so there is no conversion anywhere, and the
// only thing still unreachable from outside is gen.New, which is the thing that
// had to be unreachable.
//
// Adding a query means adding its row or params alias here. That is deliberate
// friction, and it is one line: the set of database types the rest of the
// service can see is a decision, not a side effect of what sqlc happened to
// emit.
type (
	// Querier is the set of generated queries, bound to a tenant-scoped
	// transaction. Callers never construct one; [Store.WithTenant] hands it
	// over. It is an interface, so tests outside this package can substitute a
	// fake without a database.
	Querier = gen.Querier

	// Organization is a tenant. Its id is the value every other table carries
	// as tenant_id.
	Organization = gen.Organization

	// User is a global identity, visible to a tenant only through a membership.
	User = gen.User

	// Membership joins a user to a tenant and carries their role in it.
	Membership = gen.Membership

	// Project is the top of the tenant-scoped hierarchy.
	Project = gen.Project

	// Board belongs to a project.
	Board = gen.Board

	// Column belongs to a board and orders cards within it.
	Column = gen.Column

	// Card belongs to a column.
	Card = gen.Card

	// CreateProjectParams are the arguments to Querier.CreateProject. There is
	// no tenant_id field: the tenant comes from the transaction.
	CreateProjectParams = gen.CreateProjectParams

	// CreateOrganizationParams are the arguments to
	// Querier.CreateOrganization. Like the others it carries no id: an
	// organization *is* its tenant, so its primary key is
	// current_tenant_id() and comes from the transaction.
	CreateOrganizationParams = gen.CreateOrganizationParams

	// CreateMembershipParams are the arguments to Querier.CreateMembership.
	// Like CreateProjectParams it has no tenant_id field: the tenant comes from
	// the transaction, so an admin cannot add a member to another organization.
	CreateMembershipParams = gen.CreateMembershipParams

	// ListMembersRow is one row of Querier.ListMembers — a membership joined to
	// the user it refers to.
	ListMembersRow = gen.ListMembersRow
)
