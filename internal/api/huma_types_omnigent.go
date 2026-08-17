package api

// OmnigentStatusListInput is the Huma input for the paginated, non-secret
// Omnigent status projection of a city.
type OmnigentStatusListInput struct {
	CityScope
	PaginationParam
}
