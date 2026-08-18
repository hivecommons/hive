package visualhive

import (
	"reflect"
	"testing"

	"github.com/kubestellar/hive/pkg/beads"
)

func TestCloneBatchInputPreservesNilAndEmptyCollectionIdentity(t *testing.T) {
	empty := beads.BatchInput{
		DependsOn: []string{},
		Metadata: map[string]interface{}{
			"interface_map": map[string]interface{}{},
			"string_map":    map[string]string{},
			"strings":       []string{},
			"integers":      []int{},
			"interfaces":    []interface{}{},
			"maps":          []map[string]interface{}{},
		},
	}
	clonedEmpty := cloneBatchInput(empty)
	if !reflect.DeepEqual(clonedEmpty, empty) || clonedEmpty.DependsOn == nil || clonedEmpty.Metadata["strings"].([]string) == nil ||
		clonedEmpty.Metadata["integers"].([]int) == nil || clonedEmpty.Metadata["interfaces"].([]interface{}) == nil || clonedEmpty.Metadata["maps"].([]map[string]interface{}) == nil ||
		clonedEmpty.Metadata["interface_map"].(map[string]interface{}) == nil || clonedEmpty.Metadata["string_map"].(map[string]string) == nil {
		t.Fatalf("deep clone changed verified empty collection identity: input=%#v clone=%#v", empty, clonedEmpty)
	}

	nilCollections := beads.BatchInput{Metadata: map[string]interface{}{
		"interface_map": map[string]interface{}(nil), "string_map": map[string]string(nil), "strings": []string(nil),
		"integers": []int(nil), "interfaces": []interface{}(nil), "maps": []map[string]interface{}(nil),
	}}
	clonedNil := cloneBatchInput(nilCollections)
	if !reflect.DeepEqual(clonedNil, nilCollections) || clonedNil.DependsOn != nil || clonedNil.Metadata["strings"].([]string) != nil ||
		clonedNil.Metadata["integers"].([]int) != nil || clonedNil.Metadata["interfaces"].([]interface{}) != nil || clonedNil.Metadata["maps"].([]map[string]interface{}) != nil ||
		clonedNil.Metadata["interface_map"].(map[string]interface{}) != nil || clonedNil.Metadata["string_map"].(map[string]string) != nil {
		t.Fatalf("deep clone changed verified nil collection identity: input=%#v clone=%#v", nilCollections, clonedNil)
	}
	if cloned := cloneBatchInput(beads.BatchInput{}); cloned.Metadata != nil || cloned.DependsOn != nil {
		t.Fatalf("deep clone populated absent top-level collections: %#v", cloned)
	}
}
