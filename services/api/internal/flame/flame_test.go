package flame

import (
	"fmt"
	"reflect"
	"testing"
)

func st(v int64, frames ...string) Stack { return Stack{Frames: frames, Value: v} }

// check verifies value = self + Σ children for every node.
func check(t *testing.T, n *Node) {
	t.Helper()
	sum := n.Self
	for _, c := range n.Children {
		sum += c.Value
		check(t, c)
	}
	if sum != n.Value {
		t.Errorf("node %s: value %d != self %d + children", n.Name, n.Value, n.Self)
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name   string
		stacks []Stack
		want   *Node
	}{
		{"empty", nil, &Node{Name: "all", Children: []*Node{}}},
		{"merge and self", []Stack{st(3, "main", "a"), st(2, "main", "b"), st(1, "main"), st(4, "main", "a"), st(0, "zero"), st(-5, "neg")},
			&Node{Name: "all", Value: 10, Children: []*Node{
				{Name: "main", Value: 10, Self: 1, Children: []*Node{
					{Name: "a", Value: 7, Self: 7, Children: []*Node{}},
					{Name: "b", Value: 2, Self: 2, Children: []*Node{}},
				}},
			}}},
		{"pruned below 0.05%", []Stack{st(100000, "main", "hot"), st(10, "main", "cold1"), st(20, "main", "cold2"), st(10, "tiny")},
			&Node{Name: "all", Value: 100040, Children: []*Node{
				{Name: "main", Value: 100030, Children: []*Node{
					{Name: "hot", Value: 100000, Self: 100000, Children: []*Node{}},
					{Name: "[pruned]", Value: 30, Self: 30, Children: []*Node{}},
				}},
				{Name: "[pruned]", Value: 10, Self: 10, Children: []*Node{}},
			}}},
	}
	for _, tc := range tests {
		got := Build(tc.stacks)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, dump(got), dump(tc.want))
		}
		check(t, got)
	}
}

func dump(n *Node) string {
	s := fmt.Sprintf("%s(%d/%d)[", n.Name, n.Value, n.Self)
	for _, c := range n.Children {
		s += dump(c)
	}
	return s + "]"
}

func TestBuildMaxNodes(t *testing.T) {
	// 1000 distinct 10-frame chains (0.1 % each): every node is above the threshold, so only the node limit prunes
	var stacks []Stack
	for i := range 1000 {
		frames := []string{fmt.Sprintf("p%d", i%3)}
		for d := range 9 {
			frames = append(frames, fmt.Sprintf("f%d-%d", i, d))
		}
		stacks = append(stacks, st(9, frames...))
	}
	got := Build(stacks)
	if n := Count(got); n > MaxNodes || n < MaxNodes-10 {
		t.Errorf("nodes %d, want ≤ %d and close to it", n, MaxNodes)
	}
	check(t, got)
	if got.Value != 9000 || Count(Build(stacks[:10])) != 1+3+90 {
		t.Errorf("root value %d", got.Value)
	}
}

func TestDiff(t *testing.T) {
	target := []Stack{st(60, "main", "new"), st(40, "main", "old")}
	base := []Stack{st(200, "main", "old"), st(1, "main", "gone")}
	got := Diff(target, base)
	want := &DiffNode{Name: "all", Value: 100, BaseValue: 201, Children: []*DiffNode{
		{Name: "main", Value: 100, BaseValue: 201, Children: []*DiffNode{
			{Name: "new", Value: 60, Self: 60, Children: []*DiffNode{}},
			{Name: "old", Value: 40, Self: 40, BaseValue: 200, BaseSelf: 200, Children: []*DiffNode{}},
			{Name: "gone", BaseValue: 1, BaseSelf: 1, Children: []*DiffNode{}},
		}},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("diff = %+v", got.Children[0].Children)
	}
	// a node tiny in the target but large in the base is kept
	got = Diff([]Stack{st(1e6, "a"), st(1, "b")}, []Stack{st(10, "b")})
	if len(got.Children) != 2 || got.Children[1].Name != "b" {
		t.Errorf("base-only share pruned: %+v", got.Children)
	}
	// tiny on both sides → [pruned] with both values
	got = Diff([]Stack{st(1e6, "a"), st(1, "b")}, []Stack{st(1e6, "a"), st(2, "b")})
	if len(got.Children) != 2 || got.Children[1].Name != PrunedName || got.Children[1].Value != 1 || got.Children[1].BaseValue != 2 {
		t.Errorf("pruned diff: %+v", got.Children)
	}
}

func TestTop(t *testing.T) {
	stacks := []Stack{
		st(10, "main", "rec", "rec", "rec"), // recursion: rec total counted once
		st(5, "main", "work"),
		st(2, "main"),
		st(3, "main", "rec", "work"),
	}
	got := Top(stacks, 0)
	want := []Fn{
		{Name: "rec", Self: 10, Total: 13},
		{Name: "work", Self: 8, Total: 8},
		{Name: "main", Self: 2, Total: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("top = %+v", got)
	}
	if got := Top(stacks, 1); len(got) != 1 || got[0].Name != "rec" {
		t.Errorf("limit: %+v", got)
	}
	if Total(stacks) != 20 {
		t.Errorf("total %d", Total(stacks))
	}
}
