package omnigent

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrProfileSecretReferenceUnavailable reports that a selected catalog profile
// names a logical credential absent from the worker's runtime configuration.
var ErrProfileSecretReferenceUnavailable = errors.New("profile secret reference unavailable")

// ProfileSecretReferenceError identifies a non-secret profile/reference pair
// that could not be projected. It intentionally excludes source and destination
// paths and credential values.
type ProfileSecretReferenceError struct {
	ProfileID   string
	ReferenceID string
	Err         error
}

func (e *ProfileSecretReferenceError) Error() string {
	return fmt.Sprintf("omnigent profile %q secret reference %q: %v", e.ProfileID, e.ReferenceID, e.Err)
}

// Unwrap supports typed recovery without exposing credential material.
func (e *ProfileSecretReferenceError) Unwrap() error { return e.Err }

// ProfileCredentialProjection is one selected profile's provider-confined
// credential plan. References contain identities and destinations but never
// resolved values or sources belonging to another runtime provider.
type ProfileCredentialProjection struct {
	ProfileID  string
	Harness    string
	Backend    string
	Blurb      string
	References []runtime.SecretReference
}

// ProjectProfileCredentials selects exactly one profile and its ordered
// fallback chain, then resolves each catalog-owned logical reference from the
// worker configuration. Unselected profile references are never returned.
func (c *Catalog) ProjectProfileCredentials(profileID string, provider runtime.SecretProvider, refs []runtime.SecretReference) ([]ProfileCredentialProjection, error) {
	if _, err := runtime.SelectSecretReferences(provider, nil); err != nil {
		return nil, err
	}
	if err := runtime.ValidateSecretReferences(refs); err != nil {
		return nil, err
	}
	chain, err := c.Chain(profileID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]runtime.SecretReference, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	projections := make([]ProfileCredentialProjection, 0, len(chain))
	for _, profile := range chain {
		owned := make([]runtime.SecretReference, 0, len(profile.SecretReferences))
		for _, referenceID := range profile.SecretReferences {
			ref, ok := byID[referenceID]
			if !ok {
				return nil, &ProfileSecretReferenceError{
					ProfileID: profile.ID, ReferenceID: referenceID, Err: ErrProfileSecretReferenceUnavailable,
				}
			}
			owned = append(owned, ref)
		}
		selected, err := runtime.SelectSecretReferences(provider, owned)
		if err != nil {
			return nil, &ProfileSecretReferenceError{ProfileID: profile.ID, ReferenceID: secretProviderErrorReferenceID(err), Err: err}
		}
		projections = append(projections, ProfileCredentialProjection{
			ProfileID: profile.ID, Harness: profile.Harness, Backend: profile.Backend,
			Blurb: profile.Blurb, References: selected,
		})
	}
	return projections, nil
}

func secretProviderErrorReferenceID(err error) string {
	var providerErr *runtime.SecretProviderError
	if errors.As(err, &providerErr) {
		return providerErr.ReferenceID
	}
	return ""
}
