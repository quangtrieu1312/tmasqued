package request

// UpsertClients creates or updates clients by name (POST /clients).
type UpsertClients struct {
    Names []string `json:"names"`
}

// RoleIDs is the body for linking or unlinking roles on a client by id
// (POST / DELETE /clients/{id}/roles).
type RoleIDs struct {
    RoleIDs []int64 `json:"role_ids"`
}

// RoleNames is the body for linking or unlinking roles on a client by name
// (POST / DELETE /clients/by-name/{name}/roles).
type RoleNames struct {
    RoleNames []string `json:"role_names"`
}
