package docs

import (
	"testing"

	"go.yaml.in/yaml/v4"
)

// The status schema documents what an empty domain means; an unquoted flow
// scalar with a comma would truncate the sentence and add a stray key.
func TestOpenAPIStatusDomainDescription(t *testing.T) {
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]map[string]any `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(OpenAPI, &spec); err != nil {
		t.Fatal(err)
	}
	domain := spec.Components.Schemas["Status"].Properties["domain"]
	const want = "Assigned domain ID, or an empty string before assignment."
	if len(domain) != 2 || domain["type"] != "string" || domain["description"] != want {
		t.Fatalf("Status.domain = %#v, want type string and description %q", domain, want)
	}
}
