package herdr

import (
	"fmt"
	"strings"
	"time"
)

const (
	metaLastActivity = "GC_HERDR_LAST_ACTIVITY"
	metaLastStatus   = "GC_HERDR_LAST_STATUS"

	// The controller normally observes liveness once per reconcile tick. Keep
	// repeated "working" observations useful as a heartbeat without allowing a
	// faster caller to turn the sidecar into a write loop.
	activityHeartbeatInterval = 5 * time.Second
)

// GetLastActivity returns the durable time of the last observed Herdr status
// transition, working heartbeat, start, or successful nudge. It performs only a
// sidecar read: session/API list calls never spawn another Herdr process.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	raw, err := p.GetMeta(name, metaLastActivity)
	if err != nil || raw == "" {
		return time.Time{}, err
	}
	stamp, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing last activity for %q: %w", name, err)
	}
	return stamp, nil
}

// markActivity records an activity fact without changing the last observed
// Herdr status. Start uses it because a successfully launched process is real
// runtime activity. Nudge deliberately does not: delivery is controller input,
// not proof that the agent reacted, and must not hide an unresponsive session.
func (p *Provider) markActivity(name string) error {
	p.activityMu.Lock()
	defer p.activityMu.Unlock()
	return p.writeActivity(name, p.now().UTC())
}

// recordObservedActivity projects Herdr's status signal into the durable
// provider sidecar. Transitions are stamped once; a working status is also
// refreshed periodically so a long turn remains visibly active. Stable idle,
// done, unknown, and bound-only observations are read-only after their initial
// stamp.
func (p *Provider) recordObservedActivity(name, status string) error {
	p.activityMu.Lock()
	defer p.activityMu.Unlock()

	status = strings.ToLower(strings.TrimSpace(status))
	if status == "" {
		status = "unknown"
	}
	previous, err := p.GetMeta(name, metaLastStatus)
	if err != nil {
		return err
	}
	last, err := p.GetLastActivity(name)
	if err != nil {
		return err
	}
	now := p.now().UTC()
	transitioned := previous != status
	heartbeatDue := activityStatusWorking(status) &&
		(last.IsZero() || now.Sub(last) >= activityHeartbeatInterval)
	if last.IsZero() || transitioned || heartbeatDue {
		if err := p.writeActivity(name, now); err != nil {
			return err
		}
	}
	if transitioned {
		if err := p.SetMeta(name, metaLastStatus, status); err != nil {
			return fmt.Errorf("writing observed status for %q: %w", name, err)
		}
	}
	return nil
}

func (p *Provider) writeActivity(name string, stamp time.Time) error {
	if err := p.SetMeta(name, metaLastActivity, stamp.Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("writing last activity for %q: %w", name, err)
	}
	return nil
}

func activityStatusWorking(status string) bool {
	switch status {
	case "working", "running", "active", "thinking":
		return true
	default:
		return false
	}
}
