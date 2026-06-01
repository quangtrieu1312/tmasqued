package service

import (
	"context"
    "github.com/quangtrieu1312/tmasqued/domain"
    "github.com/quangtrieu1312/tmasqued/repository"
    "github.com/quangtrieu1312/tmasqued/request"
)

func GetAllResources(ctx context.Context) (*[]domain.Resource, error) {
    return repository.GetAllResources()
}
func GetResourceByID(ctx context.Context, resourceID int64) (*domain.Resource, error) {
    return repository.GetResourceByID(resourceID)
}
func UpsertResources(ctx context.Context, resources []request.Resource) (*[]int64, error) {
    return repository.UpsertResources(resources)
}
func UpdateResourceName(ctx context.Context, resourceID int64, newName string) (bool, error) {
    return repository.UpdateResourceName(resourceID, newName)
}

// DeleteResources removes resources and returns the IDs actually deleted.
func DeleteResources(ctx context.Context, resourceIDs []int64) (*[]int64, error) {
    return repository.DeleteResources(resourceIDs)
}
