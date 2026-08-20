package v1alpha1

// Direwolf label key constants.
// These are used across all files, to avoid reusing code and having the linter throw warnings.
const (
	// LabelApp is the name of the application resource.
	LabelApp = "direwolf/app"

	// LabelProfile is the user-paired profile resource name.
	LabelProfile = "direwolf/profile"

	// LabelUser is the user name from the pairing resource.
	LabelUser = "direwolf/user"

	// LabelPairing is the name from the pairing resource.
	LabelPairing = "direwolf/pairing"

	// LabelLobby is the name from the lobby resource.
	LabelLobby = "direwolf/lobby"

	// LabelLobbyType indicates whether the lobby supports single or multiple users.
	LabelLobbyType = "direwolf/lobby-type"

	// LabelSession is the session resource name.
	LabelSession = "direwolf/session"

	// LabelNode indicates whether the node is ready to stream applications.
	LabelNode = "direwolf/node"
)

// LobbyType values.
const (
	LobbyTypeSingle   = "single"
	LobbyTypeMultiple = "multiple"
)

// Node readiness values.
const (
	NodeReady    = "ready"
	NodeNotReady = "not-ready"
)
