package praector

import "testing"

func TestDependencyPath(t *testing.T) {
	edges := [][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}}
	p := dependencyPathInEdges(edges, "A", "D")
	if len(p) != 4 || p[0] != "A" || p[3] != "D" {
		t.Fatalf("unexpected path: %v", p)
	}
	if p := dependencyPathInEdges(edges, "D", "A"); p != nil {
		t.Fatalf("unexpected reverse path: %v", p)
	}
}
func TestDependencyCycle(t *testing.T) {
	edges := [][2]string{{"A", "B"}, {"B", "C"}, {"C", "A"}}
	c := dependencyCycle(edges)
	if len(c) < 4 || c[0] != c[len(c)-1] {
		t.Fatalf("expected closed cycle, got %v", c)
	}
}
