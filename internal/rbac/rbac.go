// Package rbac defines the roles NEXUS recognises and what each may do.
//
// The permission set is deliberately small and fixed in code rather than stored as an
// editable matrix. A control plane with three roles does not need a policy engine, and
// every authorisation decision here is a map lookup rather than a database round trip.
// If NEXUS ever needs per-resource or per-team grants, that is a schema change and a new
// decision point — not something to half-build in advance.
package rbac

import "fmt"

type Role string

const (
	RoleAdmin     Role = "ADMIN"
	RoleDeveloper Role = "DEVELOPER"
	RoleViewer    Role = "VIEWER"
)

func (r Role) Valid() bool {
	switch r {
	case RoleAdmin, RoleDeveloper, RoleViewer:
		return true
	}
	return false
}

// Permission names an action. The `resource:verb` form is what appears in audit records
// and in API error messages, so it is part of the product surface, not an internal code.
type Permission string

const (
	ProjectCreate Permission = "project:create"
	ProjectRead   Permission = "project:read"
	ProjectUpdate Permission = "project:update"
	ProjectDelete Permission = "project:delete"

	PipelineRun    Permission = "pipeline:run"
	PipelineCancel Permission = "pipeline:cancel"
	PipelineRead   Permission = "pipeline:read"

	DeploymentCreate   Permission = "deployment:create"
	DeploymentRollback Permission = "deployment:rollback"
	DeploymentRead     Permission = "deployment:read"

	EnvironmentCreate Permission = "environment:create"
	EnvironmentDelete Permission = "environment:delete"
	EnvironmentRead   Permission = "environment:read"

	SecretManage Permission = "secret:manage"
	AuditRead    Permission = "audit:read"
	WorkerRead   Permission = "worker:read"
	MemberManage Permission = "member:manage"
)

// grants is the whole authorisation model.
//
// Two boundaries are worth pointing at. A developer can deploy and roll back but cannot
// delete a project or an environment, because those destroy history and running systems
// rather than changing them. Only an admin manages secrets, because the ability to read
// or rotate a secret reference is effectively the ability to reach whatever it protects.
var grants = map[Role]map[Permission]bool{
	RoleAdmin: {
		ProjectCreate: true, ProjectRead: true, ProjectUpdate: true, ProjectDelete: true,
		PipelineRun: true, PipelineCancel: true, PipelineRead: true,
		DeploymentCreate: true, DeploymentRollback: true, DeploymentRead: true,
		EnvironmentCreate: true, EnvironmentDelete: true, EnvironmentRead: true,
		SecretManage: true, AuditRead: true, WorkerRead: true, MemberManage: true,
	},
	RoleDeveloper: {
		ProjectCreate: true, ProjectRead: true, ProjectUpdate: true,
		PipelineRun: true, PipelineCancel: true, PipelineRead: true,
		DeploymentCreate: true, DeploymentRollback: true, DeploymentRead: true,
		EnvironmentCreate: true, EnvironmentRead: true,
		AuditRead: true, WorkerRead: true,
	},
	RoleViewer: {
		ProjectRead: true, PipelineRead: true, DeploymentRead: true,
		EnvironmentRead: true, WorkerRead: true,
	},
}

// Can reports whether a role holds a permission.
func Can(role Role, p Permission) bool {
	return grants[role][p]
}

// Permissions lists what a role may do, for the API to return to a client so a UI can
// hide actions the user cannot take. Hiding is a courtesy; the server still checks.
func Permissions(role Role) []Permission {
	set := grants[role]
	out := make([]Permission, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	return out
}

// ForbiddenError is returned when a permission check fails. It names the permission so
// the API response can tell the caller precisely what they lack.
type ForbiddenError struct {
	Role       Role
	Permission Permission
}

func (e *ForbiddenError) Error() string {
	return fmt.Sprintf("role %s does not have permission %s", e.Role, e.Permission)
}

// Require returns a ForbiddenError unless the role holds the permission.
func Require(role Role, p Permission) error {
	if Can(role, p) {
		return nil
	}
	return &ForbiddenError{Role: role, Permission: p}
}
