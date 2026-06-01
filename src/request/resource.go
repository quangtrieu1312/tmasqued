package request

type Resource struct {
	Name string `json:"name"`
	Value string `json:"value"`
}

// UpsertResources creates or updates resources (POST /resources).
type UpsertResources struct {
    Resources []Resource `json:"resources"`
}
