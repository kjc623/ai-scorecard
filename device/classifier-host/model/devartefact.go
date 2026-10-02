package model

import "encoding/json"

// DevArtefactJSON is the **development** artefact: a small hand-written linear scorer used by
// the test corpus, the equivalence harness and the latency measurement.
//
// It is not a production model and must not be presented as one. Training and shipping the real
// artefact is a content pipeline (signed with the release, §9.6), and this build round has no
// labelled data and no way to fetch any; what this fixture proves is the *mechanism*: an
// artefact with a signed digest, verified before use, scored in integer arithmetic, deterministic
// across the two targets, and skippable when missing or unverified.
func DevArtefactJSON() []byte {
	a := Artefact{
		Version: "dev-artefact-1",
		Scale:   1000,
		Classes: []ClassModel{
			{
				Class: "customer_pii", Threshold: 350,
				Features: []Feature{
					{Token: "passport", Weight: 90},
					{Token: "date of birth", Weight: 220},
					{Token: "national insurance", Weight: 220},
					{Token: "social security", Weight: 200},
					{Token: "ssn", Weight: 200},
					{Token: "home address", Weight: 180},
					{Token: "email", Weight: 70},
					{Token: "phone", Weight: 60},
					{Token: "address", Weight: 50},
					{Token: "mr", Weight: 25},
					{Token: "mrs", Weight: 25},
				},
			},
			{
				Class: "source_code", Threshold: 400,
				Features: []Feature{
					{Token: "package", Weight: 80},
					{Token: "func", Weight: 120},
					{Token: "def", Weight: 120},
					{Token: "import", Weight: 70},
					{Token: "namespace", Weight: 100},
					{Token: "printf", Weight: 100},
					{Token: "include", Weight: 120},
					{Token: "struct", Weight: 90},
					{Token: "void", Weight: 80},
					{Token: "const", Weight: 50},
					{Token: "return", Weight: 40},
					{Token: "async", Weight: 60},
					{Token: "await", Weight: 60},
				},
			},
			{
				Class: "legal", Threshold: 400,
				Features: []Feature{
					{Token: "whereas", Weight: 200},
					{Token: "hereinafter", Weight: 260},
					{Token: "witnesseth", Weight: 260},
					{Token: "indemnify", Weight: 240},
					{Token: "governing law", Weight: 240},
					{Token: "jurisdiction", Weight: 160},
					{Token: "liability", Weight: 150},
					{Token: "clause", Weight: 130},
					{Token: "agreement", Weight: 120},
					{Token: "shall", Weight: 90},
					{Token: "parties", Weight: 90},
					{Token: "obligations", Weight: 80},
				},
			},
			{
				Class: "health", Threshold: 400,
				Features: []Feature{
					{Token: "diagnosis", Weight: 220},
					{Token: "prescription", Weight: 220},
					{Token: "patient", Weight: 180},
					{Token: "clinical", Weight: 140},
					{Token: "physician", Weight: 180},
					{Token: "dosage", Weight: 180},
					{Token: "symptoms", Weight: 140},
					{Token: "treatment", Weight: 120},
					{Token: "therapy", Weight: 110},
					{Token: "icd-10", Weight: 240},
					{Token: "medical", Weight: 90},
				},
			},
		},
	}
	b, err := json.Marshal(a)
	if err != nil {
		panic("model: dev artefact does not marshal: " + err.Error())
	}
	return b
}
