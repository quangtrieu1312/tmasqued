package response

// IDs is the standard response for endpoints that create, delete, or link
// entities — the IDs that were actually affected by the request.
type IDs struct {
	IDs []int64 `json:"ids"`
}
