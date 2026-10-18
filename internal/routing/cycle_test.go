package routing

import (
	"reflect"
	"testing"
)

func TestFindCycle(t *testing.T) {
	for name, test := range map[string]struct {
		edges map[string][]string
		want  []string
	}{
		"empty":    {nil, nil},
		"acyclic":  {map[string][]string{"a": {"b", "c"}, "b": {"c"}}, nil},
		"self":     {map[string][]string{"a": {"a"}}, []string{"a", "a"}},
		"two":      {map[string][]string{"a": {"b"}, "b": {"a"}}, []string{"a", "b", "a"}},
		"indirect": {map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"b"}}, []string{"b", "c", "b"}},
		"diamond":  {map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}}, nil},
	} {
		if got := FindCycle(test.edges); !reflect.DeepEqual(got, test.want) {
			t.Errorf("%s: got %v want %v", name, got, test.want)
		}
	}
}
