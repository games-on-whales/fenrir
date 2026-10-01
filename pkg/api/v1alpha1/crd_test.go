package v1alpha1

import (
	"os"
	"testing"

	sigsyaml "sigs.k8s.io/yaml"
)

type schemaProps struct {
	Properties map[string]schemaProps `json:"properties"`
	Items      *schemaProps           `json:"items"`
}

// The API server prunes every field a structural schema does not declare. An
// App's spec.template.metadata used to be a bare {type: object}, so the labels
// and annotations set there were dropped on admission and never reached the
// session pod. Guard the generated CRDs (crd:generateEmbeddedObjectMeta=true).
func TestCRDsKeepEmbeddedMetadata(t *testing.T) {
	for _, tc := range []struct {
		file string
		path []string // from spec; "[]" steps into array items
	}{
		{"apps", []string{"template"}},
		{"apps", []string{"volumeClaimTemplate"}},
		{"users", []string{"volumes", "[]", "ephemeral", "volumeClaimTemplate"}},
	} {
		data, err := os.ReadFile("../../../crds/direwolf.games-on-whales.github.io_" + tc.file + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Spec struct {
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema schemaProps `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := sigsyaml.Unmarshal(data, &crd); err != nil {
			t.Fatal(err)
		}
		node := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		for _, step := range tc.path {
			if step == "[]" {
				if node.Items == nil {
					t.Fatalf("%s %v: no array items", tc.file, tc.path)
				}
				node = *node.Items
				continue
			}
			node = node.Properties[step]
		}
		meta := node.Properties["metadata"]
		for _, key := range []string{"labels", "annotations"} {
			if _, ok := meta.Properties[key]; !ok {
				t.Errorf("%s spec.%v.metadata declares no %s: the API server prunes them", tc.file, tc.path, key)
			}
		}
	}
}
