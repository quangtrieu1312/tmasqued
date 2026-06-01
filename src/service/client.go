package service

import (
	"context"
    "github.com/quangtrieu1312/tmasqued/domain"
    "github.com/quangtrieu1312/tmasqued/repository"
)

func GetAllClients(ctx context.Context) (*[]domain.Client, error) {
    return repository.GetAllClients()
}
func GetClientByID(ctx context.Context, id int64) (*domain.Client, error) {
    return repository.GetClientByID(id)
}
// CreateClientsWithRoles creates clients + their default roles + links atomically.
func CreateClientsWithRoles(ctx context.Context, clientNames []string) (*[]int64, error) {
    return repository.CreateClientsWithRoles(clientNames)
}

// AssignRolesToClientByName links roles (by name) to a client (by name), atomically.
func AssignRolesToClientByName(ctx context.Context, clientName string, roleNames []string) (*[]int64, error) {
    return repository.AssignRolesToClientByName(clientName, roleNames)
}

// UnassignRolesToClientByName unlinks roles (by name) from a client (by name), atomically.
func UnassignRolesToClientByName(ctx context.Context, clientName string, roleNames []string) (*[]int64, error) {
    return repository.UnassignRolesToClientByName(clientName, roleNames)
}
func UpsertClients(ctx context.Context, clientNames []string) (*[]int64, error) {
	ret, err:= repository.UpsertClients(clientNames)
	if err != nil {
		return nil, err
	}
	return ret, nil
}
func AssignIPToClient(ctx context.Context, clientID int64) (string, error) {
    return repository.AssignIPToClient(clientID)
}

// DeleteClients removes clients and returns the IDs actually deleted.
func DeleteClients(ctx context.Context, clientIDs []int64) (*[]int64, error) {
    return repository.DeleteClients(clientIDs)
}

// UnassignRolesToClients unlinks roles from a client and returns the role IDs removed.
func UnassignRolesToClients(ctx context.Context, roleIDs []int64, clientIDs []int64) (*[]int64, error) {
    return repository.UnassignRolesToClients(roleIDs, clientIDs)
}

// AssignRolesToClients links roles to a client and returns the role IDs newly linked.
func AssignRolesToClients(ctx context.Context, roleIDs []int64, clientIDs []int64) (*[]int64, error) {
    return repository.AssignRolesToClients(roleIDs, clientIDs)
}

// UpdateClientName renames a client (and cascades to its same-named default role).
func UpdateClientName(ctx context.Context, clientID int64, newName string) (bool, error) {
    return repository.UpdateClientName(clientID, newName)
}

// GetClientRoles returns the roles linked to a client (GET /clients/{id}/roles).
func GetClientRoles(ctx context.Context, clientID int64) (*[]domain.Role, error) {
    return repository.GetClientRoles(clientID)
}

// GetClientResources returns a client's effective resources — the union of the
// resources granted by its roles (GET /clients/{id}/resources).
func GetClientResources(ctx context.Context, clientID int64) (*[]domain.Resource, error) {
    return repository.GetClientResources(clientID)
}
