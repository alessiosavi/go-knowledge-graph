package graph

// Expose the production queries only to the external integration tests.
const (
	EntityByIDQuery  = entityByIDQuery
	EntityByKeyQuery = entityByKeyQuery
)
