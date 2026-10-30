package server

import (
	"slices"
	"time"

	"github.com/basecamp/kamal-proxy/internal/metrics"
)

// TargetDescription is what list --json reports for one target. Slot says which
// of the service's load balancers it belongs to, so that State stays the target's
// plain state whichever one that is.
type TargetDescription struct {
	Address  string            `json:"address"`
	Slot     string            `json:"slot"`
	State    string            `json:"state"`
	InFlight int               `json:"in_flight"`
	Queue    *QueueDescription `json:"queue"`
}

func (ts TargetSlot) String() string {
	if ts == TargetSlotRollout {
		return "rollout"
	}
	return "active"
}

func (tl TargetList) describe(slot TargetSlot) []TargetDescription {
	now := time.Now()
	descriptions := make([]TargetDescription, 0, len(tl))

	for _, target := range tl {
		queue := target.queue.Snapshot(now)

		descriptions = append(descriptions, TargetDescription{
			Address:  target.Address(),
			Slot:     slot.String(),
			State:    target.State().String(),
			InFlight: target.InFlight(),
			Queue:    &queue,
		})
	}

	return descriptions
}

// InFlight is how many requests this target is holding right now, streams and
// upgraded connections included.
func (t *Target) InFlight() int {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	return len(t.inflight)
}

// publishInFlight runs under the inflight lock: emitted outside it, two requests
// ending together could leave the gauge on the older of the two counts.
func (t *Target) publishInFlight(service string) {
	if !t.metricsForgotten {
		metrics.Tracker.SetTargetInFlight(service, t.Address(), len(t.inflight))
	}
}

func (t *Target) trackRequest(service string, queue, ttfb time.Duration, source string) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if !t.metricsForgotten {
		metrics.Tracker.TrackTargetRequest(service, t.Address(), queue, ttfb, source)
	}
}

// forgetMetrics deletes this target's series for good. A target dropped from its
// load balancer can still be finishing requests -- a removed service is not
// drained at all, and a drain gives up on what outlives its timeout -- and any
// emission after this would bring back a series nothing will delete again. The
// flag and the deletion share the lock so that no emission can slip between them.
func (t *Target) forgetMetrics(service string) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	t.metricsForgotten = true
	metrics.Tracker.ForgetTarget(service, t.Address())
}

// allTargets is every target the service has, in either slot.
func (s *Service) allTargets() TargetList {
	s.serviceLock.RLock()
	defer s.serviceLock.RUnlock()

	targets := TargetList{}
	if s.active != nil {
		targets = append(targets, s.active.Targets()...)
	}
	if s.rollout != nil {
		targets = append(targets, s.rollout.Targets()...)
	}

	return targets
}

// forgetDroppedTargets deletes the series of the addresses a replaced load
// balancer took with it. It is a set difference, because a deploy usually keeps
// some of its addresses, and those keep their history. It is taken against both
// of the service's slots, because an address leaving one can be serving in the
// other. Not in LoadBalancer.Dispose: that runs on failed deploys as well, when
// the replaced balancer is still the one serving.
func (r *Router) forgetDroppedTargets(name string, replaced *LoadBalancer) {
	kept := []string{}
	if service := r.serviceForName(name); service != nil {
		kept = service.allTargets().Names()
	}

	for _, target := range replaced.Targets() {
		if !slices.Contains(kept, target.Address()) {
			target.forgetMetrics(name)
		}
	}
}
