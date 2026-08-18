package api

// OmnigentStatusListInput is the Huma input for the paginated, non-secret
// Omnigent status projection of a city.
type OmnigentStatusListInput struct {
	CityScope
	PaginationParam
}

// OmnigentCapsuleStateInspectInput selects the current city's non-secret
// provider-state reconciliation report.
type OmnigentCapsuleStateInspectInput struct {
	CityScope
}

// OmnigentCapsuleStatePurgeBody chooses preview or authorized mutation.
type OmnigentCapsuleStatePurgeBody struct {
	_      struct{} `json:"-" additionalProperties:"false"`
	DryRun bool     `json:"dry_run" doc:"When true, perform fresh safety checks without recording authorization or deleting state."`
}

// OmnigentCapsuleStatePurgeInput targets one Gas City session allocation.
type OmnigentCapsuleStatePurgeInput struct {
	CityScope
	ID   string `path:"id" doc:"Closed Gas City session ID whose capsule state should be previewed or purged."`
	Body OmnigentCapsuleStatePurgeBody
}

// OmnigentCapsuleStateItem is one non-secret reconciliation result.
type OmnigentCapsuleStateItem struct {
	SessionID string `json:"session_id" doc:"Gas City session ID, or provider-recorded session ID for an orphan."`
	Action    string `json:"action" enum:"retained,retained_orphan,would_purge,purged,purge_recorded,conflict,missing"`
	Reason    string `json:"reason" doc:"Stable non-secret explanation for the factual action."`
}

// OmnigentCapsuleStateReportBody is a stable inspect, preview, or purge
// response. Foreign-city identities are counted but never disclosed.
type OmnigentCapsuleStateReportBody struct {
	DryRun         bool                       `json:"dry_run"`
	Items          []OmnigentCapsuleStateItem `json:"items"`
	IgnoredForeign int                        `json:"ignored_foreign" minimum:"0"`
}

// OmnigentCapsuleStateReportOutput wraps a capsule-state report.
type OmnigentCapsuleStateReportOutput struct {
	Body OmnigentCapsuleStateReportBody
}
