package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestDefaultClientTimeoutAccommodatesFederatedReads guards the ceiling that
// governs the control-plane read paths. ListBeads/GetBead/GetStatus/
// ListMailInbox pass context.Background(), so the HTTP client's overall timeout
// is their only deadline. Those endpoints federate the city store plus every
// rig store, and a dolt-backed rig store can take several seconds; a too-tight
// ceiling false-times-out healthy-but-slow federated reads. 10s was too tight
// once a federated read measured ~10s — keep meaningful headroom over the
// federated read cost.
func TestDefaultClientTimeoutAccommodatesFederatedReads(t *testing.T) {
	const minFederatedReadBudget = 30 * time.Second
	if defaultClientTimeout < minFederatedReadBudget {
		t.Fatalf("defaultClientTimeout = %v, want >= %v to cover federated multi-store control-plane reads",
			defaultClientTimeout, minFederatedReadBudget)
	}
}

func TestGetStatusContextClassifiesSupervisorCityNotRunning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": http.StatusNotFound,
			"title":  "Not Found",
			"detail": CityNotFoundOrNotRunningDetail("bounded-city"),
		})
	}))
	defer srv.Close()

	c := NewCityScopedClient(srv.URL, "bounded-city")
	_, err := c.GetStatusContext(context.Background())
	if !IsCityNotRunningError(err) {
		t.Fatalf("GetStatusContext error = %T %v, want typed city-not-running error", err, err)
	}
}

func TestListCitiesContextHonorsCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewCityScopedClient(srv.URL, "bounded-city")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.ListCitiesContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ListCitiesContext error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("ListCitiesContext took %s, want caller deadline to bound request", elapsed)
	}
}

func TestGetStatusContextHonorsCallerDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := NewCityScopedClient(srv.URL, "bounded-city")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.GetStatusContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("GetStatusContext error = %v, want context deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("GetStatusContext took %s, want caller deadline to bound request", elapsed)
	}
}
