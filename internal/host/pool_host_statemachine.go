package host

import (
	"github.com/filanov/stateswitch"
	"github.com/openshift/assisted-service/models"
)

func NewPoolHostStateMachine(sm stateswitch.StateMachine, th *transitionHandler) stateswitch.StateMachine {

	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRegisterHost,
		SourceStates: []stateswitch.State{
			"",
		},
		Condition:        th.IsUnboundHost,
		DestinationState: stateswitch.State(models.HostStatusDiscoveringUnbound),
		PostTransition:   th.PostRegisterHost,
	})

	// Register host
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRegisterHost,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDiscoveringUnbound),
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusInsufficientUnbound),
			stateswitch.State(models.HostStatusKnownUnbound),
		},
		DestinationState: stateswitch.State(models.HostStatusDiscoveringUnbound),
		PostTransition:   th.PostRegisterHost,
	})

	// Disabled host can register if it was booted, no change in the state.
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType:   TransitionTypeRegisterHost,
		SourceStates:     []stateswitch.State{stateswitch.State(models.HostStatusDisabledUnbound)},
		DestinationState: stateswitch.State(models.HostStatusDisabledUnbound),
	})

	// Disable host
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeDisableHost,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusDiscoveringUnbound),
			stateswitch.State(models.HostStatusInsufficientUnbound),
			stateswitch.State(models.HostStatusKnownUnbound),
		},
		DestinationState: stateswitch.State(models.HostStatusDisabledUnbound),
		PostTransition:   th.PostDisableHost,
	})

	// Enable host
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeEnableHost,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDisabledUnbound),
		},
		DestinationState: stateswitch.State(models.HostStatusDiscoveringUnbound),
		PostTransition:   th.PostEnableHost,
	})

	// Bind host
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeBindHost,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusKnownUnbound),
		},
		DestinationState: stateswitch.State(models.HostStatusBinding),
		PostTransition:   th.PostBindHost,
	})

	// Refresh host

	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRefresh,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDiscoveringUnbound),
			stateswitch.State(models.HostStatusInsufficientUnbound),
			stateswitch.State(models.HostStatusKnownUnbound),
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusUnbinding),
		},
		Condition:        stateswitch.Not(If(IsConnected)),
		DestinationState: stateswitch.State(models.HostStatusDisconnectedUnbound),
		PostTransition:   th.PostRefreshHost(statusInfoDisconnected),
	})

	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRefresh,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusDiscoveringUnbound),
		},
		Condition:        stateswitch.And(If(IsConnected), stateswitch.Not(If(HasInventory))),
		DestinationState: stateswitch.State(models.HostStatusDiscoveringUnbound),
		PostTransition:   th.PostRefreshHost(statusInfoDiscovering),
	})

	var hasMinRequiredHardware = stateswitch.And(If(HasMinValidDisks), If(HasMinCPUCores), If(HasMinMemory), If(IsPlatformValid))

	// In order for this transition to be fired at least one of the validations in minRequiredHardwareValidations must fail.
	// This transition handles the case that a host does not pass minimum hardware requirements for any of the roles
	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRefresh,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusDiscoveringUnbound),
			stateswitch.State(models.HostStatusInsufficientUnbound),
			stateswitch.State(models.HostStatusKnownUnbound),
		},
		Condition: stateswitch.And(If(IsConnected), If(HasInventory),
			stateswitch.Not(hasMinRequiredHardware)),
		DestinationState: stateswitch.State(models.HostStatusInsufficientUnbound),
		PostTransition:   th.PostRefreshHost(statusInfoInsufficientHardware),
	})

	// Noop transitions
	for _, state := range []stateswitch.State{
		stateswitch.State(models.HostStatusDisabledUnbound),
		stateswitch.State(models.HostStatusBinding),
		stateswitch.State(models.HostStatusUnbinding),
	} {
		sm.AddTransition(stateswitch.TransitionRule{
			TransitionType:   TransitionTypeRefresh,
			SourceStates:     []stateswitch.State{state},
			DestinationState: state,
		})
	}

	sm.AddTransition(stateswitch.TransitionRule{
		TransitionType: TransitionTypeRefresh,
		SourceStates: []stateswitch.State{
			stateswitch.State(models.HostStatusDisconnectedUnbound),
			stateswitch.State(models.HostStatusDiscoveringUnbound),
			stateswitch.State(models.HostStatusInsufficientUnbound),
			stateswitch.State(models.HostStatusKnownUnbound),
		},
		Condition: stateswitch.And(If(IsConnected), If(HasInventory),
			hasMinRequiredHardware),
		DestinationState: stateswitch.State(models.HostStatusKnownUnbound),
		PostTransition:   th.PostRefreshHost(statusInfoHostReadyToBeMoved),
	})

	return sm
}
