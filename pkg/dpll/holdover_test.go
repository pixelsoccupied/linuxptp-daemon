package dpll

import (
	"testing"
	"time"

	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/config"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/event"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDPLLZeroHoldover(t *testing.T) {
	for _, source := range []event.EventSource{event.PTP4l, event.GNSS} {
		t.Run(string(source), func(t *testing.T) {
			events := make(chan event.Event, 10)
			d := &DpllConfig{
				iface:                "eth0",
				dependsOn:            []event.EventSource{source},
				LocalHoldoverTimeout: 0,
				MaxInSpecOffset:      100,
				phaseStatus:          DPLL_LOCKED,
				frequencyStatus:      DPLL_LOCKED,
				processConfig: config.ProcessConfig{
					EventChannel: events,
					GMThreshold:  config.Threshold{Max: 100},
				},
			}
			d.stateDecision()
			require.Equal(t, event.PTP_LOCKED, (<-events).Data.(*event.PTPData).State)

			d.phaseStatus = DPLL_HOLDOVER
			d.stateDecision()
			deadline := time.After(time.Second)
			for {
				select {
				case ev := <-events:
					data := ev.Data.(*event.PTPData)
					assert.NotEqual(t, event.PTP_HOLDOVER, data.State)
					if data.State == event.PTP_FREERUN {
						assert.Equal(t, int64(FaultyPhaseOffset), data.Values[event.OFFSET])
						assert.False(t, d.onHoldover)
						assert.True(t, data.SourceLost)
						assert.True(t, data.OutOfSpec)
						d.phaseStatus = DPLL_LOCKED
						d.phaseOffset = 0
						d.stateDecision()
						assert.Equal(t, event.PTP_LOCKED, (<-events).Data.(*event.PTPData).State)
						return
					}
				case <-deadline:
					t.Fatal("DPLL did not report free-run with holdover disabled")
				}
			}
		})
	}
}

func TestDPLLEventHoldoverTimeout(t *testing.T) {
	for _, timeout := range []uint64{0, 30} {
		events := make(chan event.Event, 1)
		d := &DpllConfig{
			iface:                "eth0",
			dependsOn:            []event.EventSource{event.PTP4l},
			LocalHoldoverTimeout: timeout,
			processConfig:        config.ProcessConfig{EventChannel: events},
		}
		d.sendDpllEvent()
		data := (<-events).Data.(*event.PTPData)
		assert.Equal(t, timeout, data.Values[event.LocalHoldoverTimeout])
	}
}
