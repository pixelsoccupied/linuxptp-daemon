package daemon

import (
	"sync"
	"time"
)

// ptp4lSource is the last known state of one ptp4l instance, keyed by config name.
type ptp4lSource struct {
	profile string
	locked  bool
	seenAt  time.Time // last sample or event from this source
	lostAt  time.Time // last good sample before the source was lost; zero while locked or never locked
}

// isLost reports whether the ptp4l source can no longer be trusted and, if
// so, the time of its last good sample (zero when it never locked). A locked
// source is trusted only while its last sample is younger than staleAfter;
// beyond that a silent source (e.g. SLAVE->LISTENING with no FAULTY event)
// counts as lost from its last sample.
func (s ptp4lSource) isLost(now time.Time, staleAfter time.Duration) (time.Time, bool) {
	if s.locked && now.Sub(s.seenAt) <= staleAfter {
		return time.Time{}, false
	}
	if s.locked {
		return s.seenAt, true
	}
	return s.lostAt, true
}

// ptp4lSourceTracker tracks the ptp4l instances feeding phc2sys so the
// CLOCK_REALTIME clock state can be downgraded once every ptp4l is lost.
// phc2sys reports s2 as long as it can discipline the OS clock from a PHC,
// even after ptp4l stopped disciplining that PHC, so its own servo state
// cannot tell that the time source is gone.
type ptp4lSourceTracker struct {
	sync.RWMutex
	sources map[string]ptp4lSource
	now     func() time.Time
}

var ptp4lSources *ptp4lSourceTracker

// set records a ptp4l sample or event for configName.
func (o *ptp4lSourceTracker) set(configName, profile, state string) {
	o.Lock()
	defer o.Unlock()

	prev := o.sources[configName]
	src := ptp4lSource{profile: profile, locked: state == LOCKED, seenAt: o.now(), lostAt: prev.lostAt}
	// Holdover is measured from the last good sample; repeated loss samples must not restart it.
	if prev.locked && !src.locked {
		src.lostAt = prev.seenAt
	}
	o.sources[configName] = src
}

// reset clears observations when PTP profiles are reapplied.
func (o *ptp4lSourceTracker) reset() {
	o.Lock()
	defer o.Unlock()
	o.sources = map[string]ptp4lSource{}
}

// ptp4lState reports the combined state of the ptp4l instances whose profile
// is relevant: LOCKED while any ptp4l is locked and reporting, HOLDOVER until
// holdover after the last ptp4l was lost, FREERUN after that. observed is
// false when no relevant ptp4l has reported yet.
func (o *ptp4lSourceTracker) ptp4lState(relevant func(profile string) bool, holdover time.Duration) (state string, observed bool) {
	o.RLock()
	defer o.RUnlock()

	now := o.now()
	state = FREERUN
	for _, src := range o.sources {
		if !relevant(src.profile) {
			continue
		}
		observed = true
		lostAt, lost := src.isLost(now, holdover)
		if !lost {
			return LOCKED, true
		}
		if !lostAt.IsZero() && now.Before(lostAt.Add(holdover)) {
			state = HOLDOVER
		}
	}
	return state, observed
}

// phc2sysStateFromPtp4l downgrades a LOCKED phc2sys state to the combined
// state of the ptp4l instances feeding this phc2sys process: those of its HA
// profiles, or of its own profile without HA. The phc2sys state is passed
// through unchanged when no ptp4l feeds it (e.g. a GM disciplined by ts2phc).
func (o *ptp4lSourceTracker) phc2sysStateFromPtp4l(process *ptpProcess, phc2sysState string) string {
	if phc2sysState != LOCKED {
		return phc2sysState
	}
	relevant := func(profile string) bool {
		if len(process.haProfile) > 0 {
			_, ok := process.haProfile[profile]
			return ok
		}
		return process.nodeProfile.Name != nil && profile == *process.nodeProfile.Name
	}
	holdover := time.Duration(process.ptpClockThreshold.HoldOverTimeout) * time.Second
	state, observed := o.ptp4lState(relevant, holdover)
	if !observed {
		return phc2sysState
	}
	return state
}
