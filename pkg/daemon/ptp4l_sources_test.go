package daemon

import (
	"testing"
	"time"

	ptpv1 "github.com/k8snetworkplumbingwg/ptp-operator/api/v1"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/config"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/event"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/parser"
)

const osClockHoldover = 5 * time.Second

// osClockFixture drives ptp4l and phc2sys log lines through the parser path
// with a controllable clock and reads back the CLOCK_REALTIME state metric.
type osClockFixture struct {
	t       *testing.T
	now     time.Time
	handler *event.EventHandler
	phc2sys *ptpProcess
}

func newOSClockFixture(t *testing.T, phc2sysProfile string, haProfiles map[string][]string) *osClockFixture {
	InitializeOffsetMaps()
	f := &osClockFixture{t: t, now: time.Unix(1000, 0)}
	ptp4lSources.now = func() time.Time { return f.now }
	f.handler = event.Init("test-node", false, "", nil, nil, nil, nil, nil)
	f.phc2sys = &ptpProcess{
		name:              phc2sysProcessName,
		messageTag:        "[phc2sys.4.config]",
		nodeProfile:       ptpv1.PtpProfile{Name: &phc2sysProfile},
		haProfile:         haProfiles,
		ptpClockThreshold: &ptpv1.PtpClockThreshold{HoldOverTimeout: int64(osClockHoldover / time.Second)},
		logParser:         parser.NewPhc2SysExtractor(),
	}
	return f
}

func (f *osClockFixture) ptp4l(profile, configName, line string) {
	processWithParser(&ptpProcess{
		name:              ptp4lProcessName,
		messageTag:        "[" + configName + "]",
		ifaces:            config.IFaces{{Name: "ens1f0"}},
		nodeProfile:       ptpv1.PtpProfile{Name: &profile},
		ptpClockThreshold: &ptpv1.PtpClockThreshold{MaxOffsetThreshold: 100, MinOffsetThreshold: -100},
		logParser:         parser.NewPTP4LExtractor(),
		handler:           f.handler,
	}, "ptp4l[1000.000]: ["+configName+"] "+line)
}

func (f *osClockFixture) ptp4lLocked(profile, configName string) {
	f.ptp4l(profile, configName, "master offset -5 s2 freq -3000 path delay 90")
}

func (f *osClockFixture) ptp4lFreerun(profile, configName string) {
	f.ptp4l(profile, configName, "master offset 20 s0 freq +50000 path delay 90")
}

func (f *osClockFixture) ptp4lSlave(profile, configName string) {
	f.ptp4l(profile, configName, "port 1 (ens1f0): UNCALIBRATED to SLAVE on MASTER_CLOCK_SELECTED")
}

func (f *osClockFixture) ptp4lFaulty(profile, configName string) {
	f.ptp4l(profile, configName, "port 1 (ens1f0): SLAVE to FAULTY on FAULT_DETECTED (FT_UNSPECIFIED)")
}

func (f *osClockFixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// clockRealtime feeds one phc2sys sample with the given servo state and returns the resulting metric.
func (f *osClockFixture) clockRealtime(servo string) float64 {
	processWithParser(f.phc2sys, "phc2sys[1000.100]: [phc2sys.4.config:6] CLOCK_REALTIME phc offset 3 "+servo+" freq -18342 delay 680")
	return testutil.ToFloat64(ClockState.WithLabelValues(phc2sysProcessName, NodeName, clockRealTime))
}

func (f *osClockFixture) expect(state float64, msg string) {
	f.t.Helper()
	require.Equal(f.t, state, f.clockRealtime("s2"), msg)
}

func TestOSClockStateFollowsHASources(t *testing.T) {
	f := newOSClockFixture(t, "boundary-ha", map[string][]string{"bc1": {"ens1f0"}, "bc2": {"ens2f1"}})

	f.ptp4lSlave("bc1", "ptp4l.0.config")
	f.ptp4lLocked("bc1", "ptp4l.0.config")
	f.ptp4lFreerun("bc2", "ptp4l.1.config")
	f.expect(event.ClockStateLocked, "one locked HA source keeps CLOCK_REALTIME locked")

	f.ptp4lFaulty("bc1", "ptp4l.0.config")
	f.expect(event.ClockStateHoldover, "losing the last locked HA source puts CLOCK_REALTIME in holdover")

	f.advance(osClockHoldover - time.Millisecond)
	f.ptp4lFreerun("bc2", "ptp4l.1.config")
	f.expect(event.ClockStateHoldover, "repeated freerun samples do not restart or cut short holdover")

	f.advance(time.Millisecond)
	f.expect(event.ClockStateFreerun, "CLOCK_REALTIME enters freerun when holdOverTimeout expires")

	f.ptp4lLocked("bc2", "ptp4l.1.config")
	f.expect(event.ClockStateLocked, "a recovered HA source lets CLOCK_REALTIME lock again")
}

func TestOSClockStateSilentSourceLoss(t *testing.T) {
	f := newOSClockFixture(t, "bc1", nil)

	f.ptp4lLocked("bc1", "ptp4l.0.config")
	f.advance(osClockHoldover)
	f.expect(event.ClockStateLocked, "a locked source is trusted while its last sample is within holdOverTimeout")

	f.advance(time.Millisecond)
	f.expect(event.ClockStateFreerun, "a locked source that stopped reporting is lost as of its last sample")
}

func TestOSClockStateNeverUpgradesPhc2sys(t *testing.T) {
	f := newOSClockFixture(t, "bc1", nil)

	f.ptp4lSlave("bc1", "ptp4l.0.config")
	f.ptp4lLocked("bc1", "ptp4l.0.config")
	f.ptp4lFaulty("bc1", "ptp4l.0.config")
	require.Equal(t, float64(event.ClockStateFreerun), f.clockRealtime("s0"),
		"phc2sys freerun is reported as-is even while its source is in holdover")
}

func TestOSClockStateIgnoresUnrelatedSources(t *testing.T) {
	f := newOSClockFixture(t, "boundary-ha", map[string][]string{"bc1": {"ens1f0"}, "bc2": {"ens2f1"}})

	f.ptp4lLocked("unrelated", "ptp4l.9.config")
	f.ptp4lFreerun("bc1", "ptp4l.0.config")
	f.ptp4lFreerun("bc2", "ptp4l.1.config")
	f.expect(event.ClockStateFreerun, "a locked ptp4l outside the HA group must not keep CLOCK_REALTIME locked")
}

func TestOSClockStatePassthroughWithoutPtp4lSource(t *testing.T) {
	f := newOSClockFixture(t, "grandmaster", nil)

	f.ptp4lFreerun("bc1", "ptp4l.0.config")
	f.expect(event.ClockStateLocked, "phc2sys state is unchanged when no ptp4l feeds its profile")

	f.ptp4lFreerun("grandmaster", "ptp4l.1.config")
	f.expect(event.ClockStateFreerun, "a freerun ptp4l in the phc2sys profile is honored")

	ptp4lSources.reset()
	f.expect(event.ClockStateLocked, "reset on profile reapply returns to phc2sys passthrough")
}
