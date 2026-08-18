package api

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/api/genclient"
)

func omnigentCapsuleStateReportFromGen(body *genclient.OmnigentCapsuleStateReportBody) (OmnigentCapsuleStateReportBody, error) {
	if body == nil {
		return OmnigentCapsuleStateReportBody{}, fmt.Errorf("capsule state response body is missing")
	}
	items := make([]OmnigentCapsuleStateItem, 0)
	if body.Items != nil {
		items = make([]OmnigentCapsuleStateItem, 0, len(*body.Items))
		for _, item := range *body.Items {
			items = append(items, OmnigentCapsuleStateItem{
				SessionID: item.SessionId,
				Action:    string(item.Action),
				Reason:    item.Reason,
			})
		}
	}
	return OmnigentCapsuleStateReportBody{
		DryRun:         body.DryRun,
		Items:          items,
		IgnoredForeign: int(body.IgnoredForeign),
	}, nil
}

// InspectOmnigentCapsuleState fetches a non-mutating durable session and
// provider inventory reconciliation report.
func (c *Client) InspectOmnigentCapsuleState() (OmnigentCapsuleStateReportBody, error) {
	if err := c.requireCityScope(); err != nil {
		return OmnigentCapsuleStateReportBody{}, err
	}
	resp, err := c.cw.GetV0CityByCityNameOmnigentCapsuleStateWithResponse(context.Background(), c.cityName)
	if err != nil {
		return OmnigentCapsuleStateReportBody{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return OmnigentCapsuleStateReportBody{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return OmnigentCapsuleStateReportBody{}, err
	}
	return omnigentCapsuleStateReportFromGen(resp.JSON200)
}

// PurgeOmnigentCapsuleState previews or executes explicit purge for one
// closed session. Remote clients automatically attach configured write grants.
func (c *Client) PurgeOmnigentCapsuleState(sessionID string, dryRun bool) (OmnigentCapsuleStateReportBody, error) {
	if err := c.requireCityScope(); err != nil {
		return OmnigentCapsuleStateReportBody{}, err
	}
	resp, err := c.cw.PostV0CityByCityNameOmnigentCapsuleStateByIdPurgeWithResponse(
		context.Background(),
		c.cityName,
		sessionID,
		nil,
		genclient.PostV0CityByCityNameOmnigentCapsuleStateByIdPurgeJSONRequestBody{DryRun: dryRun},
	)
	if err != nil {
		return OmnigentCapsuleStateReportBody{}, &connError{err: fmt.Errorf("request failed: %w", err)}
	}
	if resp == nil {
		return OmnigentCapsuleStateReportBody{}, &connError{err: fmt.Errorf("nil response")}
	}
	if err := apiErrorFromResponse(resp.StatusCode(), pdOf(resp)); err != nil {
		return OmnigentCapsuleStateReportBody{}, err
	}
	return omnigentCapsuleStateReportFromGen(resp.JSON200)
}
