package clock

import (
	"testing"

	fbprotocol "github.com/facebook/time/ptp/protocol"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/event"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/ipc"
	"github.com/k8snetworkplumbingwg/linuxptp-daemon/pkg/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTBCHoldoverPolicy(t *testing.T) {
	tests := []struct {
		name            string
		timeout         uint64
		configured      bool
		follower        bool
		followerTimeout uint64
		wantState       event.PTPState
		wantClass       fbprotocol.ClockClass
	}{
		{"disabled", 0, true, false, 0, event.PTP_FREERUN, protocol.ClockClassFreerun},
		{"enabled", 30, true, false, 0, event.PTP_HOLDOVER, 135},
		{"unspecified", 0, false, false, 0, event.PTP_HOLDOVER, 135},
		{"follower cannot enable holdover", 0, true, true, 30, event.PTP_FREERUN, protocol.ClockClassFreerun},
		{"follower cannot disable holdover", 30, true, true, 0, event.PTP_HOLDOVER, 135},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bc, rec := newLockedTBCClock()
			bc.leadingClockData.upstreamParentDataSet.GrandmasterClockClass = 6
			leading := makeTBCEvent(event.DPLL, event.PTP_LOCKED, 10, false)
			values := leading.Data.(*event.PTPData).Values
			values[event.LeadingSource] = true
			if tt.configured {
				values[event.LocalHoldoverTimeout] = tt.timeout
			}
			bc.AddEvent(leading)

			if tt.follower {
				follower := makeTBCEvent(event.DPLL, event.PTP_LOCKED, 10, false)
				follower.IFace = testEth1
				follower.Data.(*event.PTPData).Values[event.LeadingSource] = false
				follower.Data.(*event.PTPData).Values[event.LocalHoldoverTimeout] = tt.followerTimeout
				bc.AddEvent(follower)
			}

			result := bc.AddEvent(makeTBCEvent(event.PTP4l, event.PTP_FREERUN, 10, true))
			assert.Equal(t, tt.wantState, result.State)
			assert.Equal(t, tt.wantClass, result.ClockClass)

			if tt.wantState == event.PTP_FREERUN {
				for _, msg := range rec.messages {
					if state, ok := msg.Values.(ipc.StateValue); ok {
						assert.NotEqual(t, ipc.StateHoldover, state.State)
					}
					if class, ok := msg.Values.(ipc.ClockClassValue); ok {
						assert.NotEqual(t, uint8(135), class.ClockClass)
					}
				}
			}

			bc.AddEvent(leading)
			fillBCDataWindows(bc, 10)
			result = bc.AddEvent(makeTBCEvent(event.PTP4l, event.PTP_LOCKED, 10, false))
			require.Equal(t, event.PTP_LOCKED, result.State)
			result = bc.AddEvent(leading)
			assert.Equal(t, fbprotocol.ClockClass(6), result.ClockClass)
		})
	}
}
