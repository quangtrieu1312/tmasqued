package request

// UpsertRoles creates or updates roles by name (POST /roles).
type UpsertRoles struct {
    Names []string `json:"names"`
}

// ResourceIDs is the body for granting or revoking resources on a role by id
// (POST / DELETE /roles/{id}/resources).
type ResourceIDs struct {
    ResourceIDs []int64 `json:"resource_ids"`
}

// ResourceNames is the body for granting or revoking resources on a role by name
// (POST / DELETE /roles/by-name/{name}/resources).
type ResourceNames struct {
    ResourceNames []string `json:"resource_names"`
}
