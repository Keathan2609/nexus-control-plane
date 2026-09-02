package rbac_test

import (
	"errors"
	"testing"

	"github.com/Keathan2609/nexus/internal/rbac"
)

func TestViewerCannotMutateAnything(t *testing.T) {
	forbidden := []rbac.Permission{
		rbac.ProjectCreate, rbac.ProjectUpdate, rbac.ProjectDelete,
		rbac.PipelineRun, rbac.PipelineCancel,
		rbac.DeploymentCreate, rbac.DeploymentRollback,
		rbac.EnvironmentCreate, rbac.EnvironmentDelete,
		rbac.SecretManage, rbac.MemberManage,
	}
	for _, p := range forbidden {
		if rbac.Can(rbac.RoleViewer, p) {
			t.Errorf("a viewer must not hold %s", p)
		}
	}
}

func TestViewerCanRead(t *testing.T) {
	for _, p := range []rbac.Permission{
		rbac.ProjectRead, rbac.PipelineRead, rbac.DeploymentRead, rbac.EnvironmentRead,
	} {
		if !rbac.Can(rbac.RoleViewer, p) {
			t.Errorf("a viewer must hold %s", p)
		}
	}
}

func TestDeveloperCanDeployAndRollBack(t *testing.T) {
	for _, p := range []rbac.Permission{
		rbac.PipelineRun, rbac.PipelineCancel,
		rbac.DeploymentCreate, rbac.DeploymentRollback,
	} {
		if !rbac.Can(rbac.RoleDeveloper, p) {
			t.Errorf("a developer must hold %s", p)
		}
	}
}

// Deleting a project or an environment destroys history and running systems rather than
// changing them, so it is reserved to admins.
func TestDeveloperCannotDestroy(t *testing.T) {
	for _, p := range []rbac.Permission{rbac.ProjectDelete, rbac.EnvironmentDelete} {
		if rbac.Can(rbac.RoleDeveloper, p) {
			t.Errorf("a developer must not hold %s", p)
		}
	}
}

func TestOnlyAdminManagesSecretsAndMembers(t *testing.T) {
	for _, p := range []rbac.Permission{rbac.SecretManage, rbac.MemberManage} {
		if !rbac.Can(rbac.RoleAdmin, p) {
			t.Errorf("an admin must hold %s", p)
		}
		if rbac.Can(rbac.RoleDeveloper, p) || rbac.Can(rbac.RoleViewer, p) {
			t.Errorf("%s must be reserved to admins", p)
		}
	}
}

func TestUnknownRoleHoldsNothing(t *testing.T) {
	// A role read from a stale token or a corrupted row must fail closed.
	if rbac.Can(rbac.Role("SUPERUSER"), rbac.ProjectRead) {
		t.Fatal("an unrecognised role must hold no permissions")
	}
}

func TestRequireReturnsForbiddenErrorNamingThePermission(t *testing.T) {
	err := rbac.Require(rbac.RoleViewer, rbac.DeploymentRollback)
	if err == nil {
		t.Fatal("expected an error")
	}
	var forbidden *rbac.ForbiddenError
	if !errors.As(err, &forbidden) {
		t.Fatalf("expected a ForbiddenError, got %T", err)
	}
	if forbidden.Permission != rbac.DeploymentRollback {
		t.Fatalf("error must name the permission, got %s", forbidden.Permission)
	}
}

func TestRequireAllowsPermittedAction(t *testing.T) {
	if err := rbac.Require(rbac.RoleAdmin, rbac.SecretManage); err != nil {
		t.Fatalf("admin should be permitted: %v", err)
	}
}

func TestRoleValidity(t *testing.T) {
	for _, r := range []rbac.Role{rbac.RoleAdmin, rbac.RoleDeveloper, rbac.RoleViewer} {
		if !r.Valid() {
			t.Errorf("%s should be a valid role", r)
		}
	}
	if rbac.Role("ROOT").Valid() {
		t.Error("ROOT is not a role NEXUS recognises")
	}
}
