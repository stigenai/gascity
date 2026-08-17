package api

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/omnigent"
)

func (s *Server) humaHandleOmnigentStatusList(_ context.Context, input *OmnigentStatusListInput) (*ListOutput[omnigent.RemoteSessionStatus], error) {
	seek, err := keysetSeek(input.Cursor)
	if err != nil {
		return nil, err
	}
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("no session store configured")
	}
	records, err := omnigent.NewSessionStatusStore(store).List()
	if err != nil {
		return nil, apierr.Internal.Msg("reading Omnigent session status")
	}

	limit := input.Limit
	if limit <= 0 {
		limit = defaultPaginationLimit
	}
	if limit > maxPaginationLimit {
		limit = maxPaginationLimit
	}
	recordKey := func(i int) keysetKey {
		return keysetKey{CreatedAt: records[i].CreatedAt, ID: records[i].SessionID}
	}
	indexes := make([]int, len(records))
	for i := range indexes {
		indexes[i] = i
	}
	page, total, hasMore := resolveKeysetPage(indexes, recordKey, seek, limit)
	nextCursor := mintKeysetNextCursor(page, recordKey, hasMore)

	catalogPath := filepath.Join(s.state.CityPath(), ".gc", "services", "omnigent", "config", "profiles.yaml")
	catalog, catalogErr := omnigent.LoadCatalog(catalogPath)
	items := make([]omnigent.RemoteSessionStatus, 0, len(page))
	for _, index := range page {
		record := records[index]
		serviceState := omnigent.ServiceStateReady
		if record.Snapshot.Location == omnigent.AttachmentLocationController && !s.omnigentControllerServiceReady() {
			serviceState = omnigent.ServiceStateUnavailable
		}
		items = append(items, omnigent.ProjectRemoteSessionStatus(record, catalog, serviceState))
	}
	partialErrors := []string(nil)
	if catalogErr != nil && len(records) > 0 {
		partialErrors = []string{"Omnigent public profile catalog unavailable"}
	}
	return &ListOutput[omnigent.RemoteSessionStatus]{
		Index: s.latestIndex(), CacheAgeS: cacheAgeSeconds(store.Store),
		Body: ListBody[omnigent.RemoteSessionStatus]{
			Items: items, Total: total, NextCursor: nextCursor,
			Partial: catalogErr != nil && len(records) > 0, PartialErrors: partialErrors,
		},
	}, nil
}

func (s *Server) omnigentControllerServiceReady() bool {
	registry := s.state.ServiceRegistry()
	if registry == nil {
		return false
	}
	status, ok := registry.Get("omnigent")
	if !ok {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(status.LocalState), "ready") || strings.EqualFold(strings.TrimSpace(status.State), "ready")
}
