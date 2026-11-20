package graph

import (
	"encoding/json/v2"
	"fmt"
)

// ProductReleasePin is the SHA-256 of the pinned file. Metadata holds the same
// file bytes for the three JSON artifacts that carry cross-checks.
type ProductReleasePin struct {
	Digest   string
	Metadata []byte
}

var requiredProductReleasePins = [...]string{
	"R.json", "rf_kit.py", "score.json", "report.json", "freeze.json", "E2", "record-obs.jsonl",
}

// MissingProductReleasePins lists required names absent from the supplied pins.
func MissingProductReleasePins[V any](pins map[string]V) []string {
	var missing []string
	for _, name := range requiredProductReleasePins {
		if _, ok := pins[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func validateProductReleasePins(pins map[string]ProductReleasePin, registry, candidates string) error {
	match := func(artifact, field, got, want string) error {
		if want == "" || got != want {
			return fmt.Errorf("%w: %s %s does not match its referenced artifact", ErrProductData, artifact, field)
		}
		return nil
	}
	if err := match("pin", "R.json", pins["R.json"].Digest, registry); err != nil {
		return err
	}
	for _, name := range []string{"score.json", "report.json"} {
		var audit struct {
			RegistrySHA256          string `json:"registry_sha256"`
			ReleaseCandidatesSHA256 string `json:"release_candidates_sha256"`
			ReleaseAllowed          *bool  `json:"release_allowed"`
			OwnerDecisionRequired   *bool  `json:"owner_decision_required"`
		}
		if err := json.Unmarshal(pins[name].Metadata, &audit); err != nil {
			return fmt.Errorf("%w: invalid %s pin JSON", ErrProductData, name)
		}
		if err := match(name, "registry_sha256", audit.RegistrySHA256, registry); err != nil {
			return err
		}
		if err := match(name, "release_candidates_sha256", audit.ReleaseCandidatesSHA256, candidates); err != nil {
			return err
		}
		if name == "report.json" {
			if audit.ReleaseAllowed == nil || !*audit.ReleaseAllowed {
				return fmt.Errorf("%w: report.json requires release_allowed=true", ErrProductData)
			}
			if audit.OwnerDecisionRequired == nil || *audit.OwnerDecisionRequired {
				return fmt.Errorf("%w: report.json requires owner_decision_required=false", ErrProductData)
			}
		}
	}
	var freeze struct {
		SHA256   map[string]string `json:"sha256"`
		E2SHA256 string            `json:"e2_sha256"`
	}
	if err := json.Unmarshal(pins["freeze.json"].Metadata, &freeze); err != nil {
		return fmt.Errorf("%w: invalid freeze.json pin JSON", ErrProductData)
	}
	for _, check := range []struct{ field, got, want string }{
		{"sha256.R.json", freeze.SHA256["R.json"], registry},
		{"sha256.rf_kit.py", freeze.SHA256["rf_kit.py"], pins["rf_kit.py"].Digest},
		{"e2_sha256 (E2)", freeze.E2SHA256, pins["E2"].Digest},
	} {
		if err := match("freeze.json", check.field, check.got, check.want); err != nil {
			return err
		}
	}
	return nil
}
