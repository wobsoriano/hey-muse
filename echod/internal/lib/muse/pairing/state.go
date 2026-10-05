package pairing

// State is how far setup has come, for a screen to show.
type State int

const (
	// StateWaiting: advertising, no phone has spoken yet.
	StateWaiting State = iota
	// StateConnected: a phone is talking, and no session is confirmed.
	StateConnected
	// StateConfirmed: the encrypted session is open and the app has consented.
	StateConfirmed
	// StateProvisioning: the phone sent the tokens and they are being checked and saved.
	StateProvisioning
	// StateDone: the tokens are saved and the phone was told. Run returns nil.
	StateDone
	// StateFailed: the last attempt failed for Progress.Reason. Until the window closes the phone may
	// try again, which moves on from here.
	StateFailed
)

func (s State) String() string {
	switch s {
	case StateWaiting:
		return "waiting for phone"
	case StateConnected:
		return "phone connected"
	case StateConfirmed:
		return "confirmed"
	case StateProvisioning:
		return "provisioning"
	case StateDone:
		return "done"
	case StateFailed:
		return "failed"
	}
	return "unknown"
}

// Reason is why setup is in StateFailed.
type Reason int

const (
	ReasonNone Reason = iota
	// ReasonHandshake: the phone sent a record that did not open, or a hello with a bad key.
	ReasonHandshake
	// ReasonOffline: the device could not get online.
	ReasonOffline
	// ReasonAuthRejected: Muse did not accept the device token, or could not be asked.
	ReasonAuthRejected
	// ReasonStorage: the tokens could not be saved.
	ReasonStorage
	// ReasonWindowClosed: nobody finished setup in time.
	ReasonWindowClosed
	// ReasonCanceled: the caller stopped setup.
	ReasonCanceled
)

func (r Reason) String() string {
	switch r {
	case ReasonNone:
		return ""
	case ReasonHandshake:
		return "the phone and this device could not agree on a secure session"
	case ReasonOffline:
		return "this device is not online"
	case ReasonAuthRejected:
		return "Muse did not accept this device"
	case ReasonStorage:
		return "the pairing could not be saved"
	case ReasonWindowClosed:
		return "setup timed out"
	case ReasonCanceled:
		return "setup was stopped"
	}
	return "unknown"
}

// Progress is one step of setup, as reported to Config.Progress.
type Progress struct {
	State  State
	Reason Reason // set only in StateFailed
	Err    error  // what went wrong, when there is more to say than Reason; never holds a token
}

// trigger is something that happened that may move the State.
type trigger int

const (
	phoneWrote trigger = iota
	phoneLeft
	handshakeBegan
	phoneConfirmed
	provisionBegan
	attemptFailed
	paired
	windowClosed
)

// transitions is every legal move. A trigger with no entry for the current state changes nothing: a
// write from a phone that is already connected, or a phone leaving after a failure, which stays on
// screen until someone tries again.
var transitions = map[State]map[trigger]State{
	StateWaiting: {
		phoneWrote:   StateConnected,
		windowClosed: StateFailed,
	},
	StateConnected: {
		phoneLeft:      StateWaiting,
		phoneConfirmed: StateConfirmed,
		attemptFailed:  StateFailed,
		windowClosed:   StateFailed,
	},
	StateConfirmed: {
		phoneLeft:      StateWaiting,
		handshakeBegan: StateConnected,
		provisionBegan: StateProvisioning,
		attemptFailed:  StateFailed,
		windowClosed:   StateFailed,
	},
	StateProvisioning: {
		phoneLeft:      StateWaiting,
		handshakeBegan: StateConnected,
		paired:         StateDone,
		attemptFailed:  StateFailed,
		windowClosed:   StateFailed,
	},
	StateFailed: {
		handshakeBegan: StateConnected,
		// The session outlives a failed Wi-Fi join, so the phone may send the tokens again.
		provisionBegan: StateProvisioning,
		attemptFailed:  StateFailed,
		windowClosed:   StateFailed,
	},
	StateDone: {},
}
