package job

type State string

const (
	StateNew              State = "NEW"
	StateSubmitted        State = "SUBMITTED"
	StateDownloading      State = "DOWNLOADING"
	StateCompletedTorbox  State = "COMPLETED_TORBOX"
	StateReady            State = "READY"
	StateFailed           State = "FAILED"
	StateCanceled         State = "CANCELED"
)

func (s State) Terminal() bool {
	switch s {
	case StateReady, StateFailed, StateCanceled:
		return true
	}
	return false
}

func ActiveStates() []State {
	return []State{StateNew, StateSubmitted, StateDownloading, StateCompletedTorbox}
}

func CanTransition(from, to State) bool {
	if from == to {
		return false
	}
	switch from {
	case StateNew:
		return to == StateSubmitted || to == StateFailed || to == StateCanceled
	case StateSubmitted:
		return to == StateDownloading || to == StateCompletedTorbox || to == StateFailed || to == StateCanceled
	case StateDownloading:
		return to == StateCompletedTorbox || to == StateFailed || to == StateCanceled
	case StateCompletedTorbox:
		return to == StateReady || to == StateFailed || to == StateCanceled
	}
	return false
}
