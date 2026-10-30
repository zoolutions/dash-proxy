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
	if !t.metricsSilenced {
		metrics.Tracker.SetTargetInFlight(service, t.Address(), len(t.inflight))
	}
}

func (t *Target) trackRequest(service string, queue, ttfb time.Duration, source string) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	if !t.metricsSilenced {
		metrics.Tracker.TrackTargetRequest(service, t.Address(), queue, ttfb, source)
	}
}

// refreshInFlight publishes the count as it stands, for when something other than
// a request starting or ending has moved the gauge.
func (t *Target) refreshInFlight(service string) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	t.publishInFlight(service)
}

// silenceMetrics stops this target writing to its series, which stay. It is for a
// replaced target whose address lives on: the series now belong to its successor.
func (t *Target) silenceMetrics() {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	t.metricsSilenced = true
}

// forgetMetrics deletes this target's series for good. A target dropped from its
// load balancer can still be finishing requests -- a removed service is not
// drained at all, and a drain gives up on what outlives its timeout -- and any
// emission after this would bring back a series nothing will delete again. The
// flag and the deletion share the lock so that no emission can slip between them.
func (t *Target) forgetMetrics(service string) {
	t.inflightLock.Lock()
	defer t.inflightLock.Unlock()

	t.metricsSilenced = true
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

// liveTargets is every target a service has, or none once it is gone. The caller
// holds the router's lock.
func (r *Router) liveTargets(name string) TargetList {
	service := r.services.Get(name)
	if service == nil {
		return TargetList{}
	}

	return service.allTargets()
}

// handOverTargets runs before a replaced load balancer drains. A replaced target
// whose address lives on shares its series with its successor, and writing to
// them through the drain would overwrite the successor's count, so it goes quiet
// now and the successor reports. What it finishes during the drain goes uncounted.
func (r *Router) handOverTargets(name string, replaced *LoadBalancer) {
	r.withWriteLock(func() error {
		live := r.liveTargets(name)
		kept := live.Names()
		shared := replaced.Targets().Names()

		for _, target := range replaced.Targets() {
			if slices.Contains(kept, target.Address()) {
				target.silenceMetrics()
			}
		}

		for _, target := range live {
			if slices.Contains(shared, target.Address()) {
				target.refreshInFlight(name)
			}
		}

		return nil
	})
}

// forgetDroppedTargets deletes the series of the addresses a replaced load
// balancer took with it, once it has drained. It is a set difference, because a
// deploy usually keeps some of its addresses, and those keep their history. It is
// taken against both of the service's slots, because an address leaving one can be
// serving in the other, and under the router's lock, because a concurrent deploy
// bringing an address back would otherwise lose the series to this deletion. Not
// in LoadBalancer.Dispose: that runs on failed deploys as well, when the replaced
// balancer is still the one serving.
func (r *Router) forgetDroppedTargets(name string, replaced *LoadBalancer) {
	r.withWriteLock(func() error {
		kept := r.liveTargets(name).Names()

		for _, target := range replaced.Targets() {
			if !slices.Contains(kept, target.Address()) {
				target.forgetMetrics(name)
			}
		}

		return nil
	})
}
