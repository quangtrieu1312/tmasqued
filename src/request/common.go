package request

// UpdateName renames a single client, role, or resource
// (PATCH /clients/{id} | /roles/{id} | /resources/{id}).
type UpdateName struct {
    Name string `json:"name"`
}
