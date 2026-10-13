package rules

import _ "embed"

// The rules document and the model artefact in this directory are the ones the product ships: the
// device installer signs them into its release, and a server that classifies a provider's record of
// a prompt compiles them from here (release.Builtin).
var (
	//go:embed default.json
	BuiltinRules []byte
	//go:embed model.json
	BuiltinModel []byte
)
