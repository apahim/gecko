package conversion

import "testing"

func TestConditionFilteringPreservesIntegerPrecision(t *testing.T) {
	const exact = int64(9007199254740993)
	existing := newTestObject(withSpec(map[string]any{"integer": exact}), withStatus(map[string]any{"conditions": []any{map[string]any{"type": "HostedClusterAvailable", "observedGeneration": exact}, map[string]any{"type": "PrivateCondition", "observedGeneration": exact}}}))
	c := &Converter{}
	private := c.extractNonPublicConditions(existing, "Cluster")
	if len(private) != 1 || private[0].(map[string]any)["observedGeneration"] != exact {
		t.Fatalf("private condition integer rounded: %#v", private)
	}
	converted := existing.DeepCopy()
	if err := c.filterNonPublicConditions(converted, "Cluster"); err != nil {
		t.Fatal(err)
	}
	if converted.Object["spec"].(map[string]any)["integer"] != exact {
		t.Fatal("condition filtering rounded spec integer")
	}
	if err := c.preserveNonPublicConditions(existing, converted, "Cluster"); err != nil {
		t.Fatal(err)
	}
	if converted.Object["spec"].(map[string]any)["integer"] != exact {
		t.Fatal("condition preservation rounded spec integer")
	}
	conditions := converted.Object["status"].(map[string]any)["conditions"].([]any)
	if len(conditions) != 2 {
		t.Fatalf("conditions lost: %#v", conditions)
	}
	for _, condition := range conditions {
		if condition.(map[string]any)["observedGeneration"] != exact {
			t.Fatalf("condition integer rounded: %#v", condition)
		}
	}
}
