package service

import (
	"context"
    "github.com/quangtrieu1312/tmasqued/domain"
    "github.com/quangtrieu1312/tmasqued/repository"
)

func GetAllRoles(ctx context.Context) (*[]domain.Role, error) {
    return repository.GetAllRoles()
}
func GetRoleByID(ctx context.Context, id int64) (*domain.Role, error) {
    return repository.GetRoleByID(id)
}
// AssignResourcesToRoleByName grants resources (by name) to a role (by name), atomically.
func AssignResourcesToRoleByName(ctx context.Context, roleName string, resourceNames []string) (*[]int64, error) {
    return repository.AssignResourcesToRoleByName(roleName, resourceNames)
}

// UnassignResourcesToRoleByName revokes resources (by name) from a role (by name), atomically.
func UnassignResourcesToRoleByName(ctx context.Context, roleName string, resourceNames []string) (*[]int64, error) {
    return repository.UnassignResourcesToRoleByName(roleName, resourceNames)
}

// AssignResourcesToRoles grants resources to a role and returns the resource IDs newly granted.
func AssignResourcesToRoles(ctx context.Context, resourceIDs []int64, roleIDs []int64) (*[]int64, error) {
    return repository.AssignResourcesToRoles(resourceIDs, roleIDs)
}

// UnassignResourcesToRoles revokes resources from a role and returns the resource IDs removed.
func UnassignResourcesToRoles(ctx context.Context, resourceIDs []int64, roleIDs []int64) (*[]int64, error) {
    return repository.UnassignResourcesToRoles(resourceIDs, roleIDs)
}
func UpdateRoleName(ctx context.Context, roleID int64, newName string) (bool, error) {
    return repository.UpdateRoleName(roleID, newName)
}
func UpsertRoles(ctx context.Context, roleNames []string) (*[]int64, error) {
    return repository.UpsertRoles(roleNames)
}

// DeleteRoles removes roles and returns the IDs actually deleted.
func DeleteRoles(ctx context.Context, roleIDs []int64) (*[]int64, error) {
    return repository.DeleteRoles(roleIDs)
}
