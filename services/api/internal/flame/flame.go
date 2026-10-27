// Package flame builds flame graph trees, diff trees and top-function tables from aggregated profile stacks
// (ADR-022, contracts §24). Stacks are root first; the tree root is "all".
package flame

import (
	"container/heap"
	"sort"
)

const (
	// RootName is the root node's name.
	RootName = "all"
	// PrunedName collects the children cut by Prune.
	PrunedName = "[pruned]"
	// MinFraction: nodes below this share of the total are pruned (0.05 %).
	MinFraction = 0.0005
	// MaxNodes bounds the returned tree (including the root and the [pruned] nodes).
	MaxNodes = 5000
)

// Stack is one aggregated stack (frames root first) and its value.
type Stack struct {
	Frames []string
	Value  int64
}

// Node is a flame graph node: Value is the total of the subtree, Self the value of stacks ending here.
type Node struct {
	Name     string  `json:"name"`
	Value    int64   `json:"value"`
	Self     int64   `json:"self"`
	Children []*Node `json:"children"`
}

// DiffNode is a node of two merged trees (target Value/Self, base BaseValue/BaseSelf; 0 where absent).
type DiffNode struct {
	Name      string      `json:"name"`
	Value     int64       `json:"value"`
	Self      int64       `json:"self"`
	BaseValue int64       `json:"baseValue"`
	BaseSelf  int64       `json:"baseSelf"`
	Children  []*DiffNode `json:"children"`
}

// tree is the build structure: two value slots (0 = target, 1 = base).
type tree struct {
	name  string
	val   [2]int64
	self  [2]int64
	kids  map[string]*tree
	order []*tree
}

func newTree(name string) *tree { return &tree{name: name} }

func (t *tree) child(name string) *tree {
	if c, ok := t.kids[name]; ok {
		return c
	}
	if t.kids == nil {
		t.kids = map[string]*tree{}
	}
	c := newTree(name)
	t.kids[name] = c
	t.order = append(t.order, c)
	return c
}

func (t *tree) add(slot int, stacks []Stack) {
	for _, s := range stacks {
		if s.Value <= 0 {
			continue
		}
		n := t
		n.val[slot] += s.Value
		for _, f := range s.Frames {
			n = n.child(f)
			n.val[slot] += s.Value
		}
		n.self[slot] += s.Value
	}
}

// Total sums stack values (non-positive values are ignored, as in the tree).
func Total(stacks []Stack) int64 {
	var t int64
	for _, s := range stacks {
		if s.Value > 0 {
			t += s.Value
		}
	}
	return t
}

// selection runs the bounded expansion: nodes are included from the largest share down while the budget allows;
// children below the threshold (in every slot) are never included. weight is the node's share used for ordering.
type item struct {
	n *tree
	w float64
}
type pq []item

func (p pq) Len() int             { return len(p) }
func (p pq) Less(i, j int) bool   { return p[i].w > p[j].w }
func (p pq) Swap(i, j int)        { p[i], p[j] = p[j], p[i] }
func (p *pq) Push(x any)          { *p = append(*p, x.(item)) }
func (p *pq) Pop() any            { o := *p; x := o[len(o)-1]; *p = o[:len(o)-1]; return x }
func (t *tree) kidsList() []*tree { return t.order }

func selectNodes(root *tree, totals [2]int64, maxNodes int) map[*tree]bool {
	share := func(n *tree) float64 {
		var w float64
		for i := range 2 {
			if totals[i] > 0 {
				w = max(w, float64(n.val[i])/float64(totals[i]))
			}
		}
		return w
	}
	keep := func(n *tree) bool {
		for i := range 2 {
			if totals[i] > 0 && float64(n.val[i]) >= float64(totals[i])*MinFraction {
				return true
			}
		}
		return false
	}
	budget := maxNodes
	for range 8 {
		in := map[*tree]bool{root: true}
		h := &pq{}
		for _, c := range root.kidsList() {
			if keep(c) {
				heap.Push(h, item{c, share(c)})
			}
		}
		for h.Len() > 0 && len(in) < budget {
			it := heap.Pop(h).(item)
			in[it.n] = true
			for _, c := range it.n.kidsList() {
				if keep(c) {
					heap.Push(h, item{c, share(c)})
				}
			}
		}
		// every included node with a left-out child gets one [pruned] child
		extra := 0
		for n := range in {
			for _, c := range n.order {
				if !in[c] {
					extra++
					break
				}
			}
		}
		if len(in)+extra <= maxNodes || budget <= 1 {
			return in
		}
		budget = max(1, budget-(len(in)+extra-maxNodes))
	}
	return map[*tree]bool{root: true}
}

func sortKids[T any](kids []T, key func(T) (int64, int64, string)) {
	sort.SliceStable(kids, func(i, j int) bool {
		a1, a2, an := key(kids[i])
		b1, b2, bn := key(kids[j])
		if a1 != b1 {
			return a1 > b1
		}
		if a2 != b2 {
			return a2 > b2
		}
		return an < bn
	})
}

// Build returns the pruned flame graph of stacks (≤ MaxNodes nodes; children by value, descending).
func Build(stacks []Stack) *Node {
	root := newTree(RootName)
	root.add(0, stacks)
	in := selectNodes(root, root.val, MaxNodes)
	var conv func(t *tree) *Node
	conv = func(t *tree) *Node {
		n := &Node{Name: t.name, Value: t.val[0], Self: t.self[0], Children: []*Node{}}
		var pruned int64
		cut := false
		for _, c := range t.order {
			if in[c] {
				n.Children = append(n.Children, conv(c))
			} else {
				pruned += c.val[0]
				cut = true
			}
		}
		if cut {
			n.Children = append(n.Children, &Node{Name: PrunedName, Value: pruned, Self: pruned, Children: []*Node{}})
		}
		sortKids(n.Children, func(x *Node) (int64, int64, string) { return x.Value, 0, x.Name })
		return n
	}
	return conv(root)
}

// Diff merges the target and base trees; a node is kept when it is ≥ MinFraction of either total.
func Diff(target, base []Stack) *DiffNode {
	root := newTree(RootName)
	root.add(0, target)
	root.add(1, base)
	in := selectNodes(root, root.val, MaxNodes)
	var conv func(t *tree) *DiffNode
	conv = func(t *tree) *DiffNode {
		n := &DiffNode{Name: t.name, Value: t.val[0], Self: t.self[0], BaseValue: t.val[1], BaseSelf: t.self[1], Children: []*DiffNode{}}
		var pv, pb int64
		cut := false
		for _, c := range t.order {
			if in[c] {
				n.Children = append(n.Children, conv(c))
			} else {
				pv += c.val[0]
				pb += c.val[1]
				cut = true
			}
		}
		if cut {
			n.Children = append(n.Children, &DiffNode{Name: PrunedName, Value: pv, Self: pv, BaseValue: pb, BaseSelf: pb, Children: []*DiffNode{}})
		}
		sortKids(n.Children, func(x *DiffNode) (int64, int64, string) { return x.Value, x.BaseValue, x.Name })
		return n
	}
	return conv(root)
}

// Fn is one row of the top functions table.
type Fn struct {
	Name  string `json:"name"`
	Self  int64  `json:"self"`
	Total int64  `json:"total"`
}

// Top returns functions by self value (then total), at most limit. Total counts a stack once per function
// even when the function appears several times in it (recursion).
func Top(stacks []Stack, limit int) []Fn {
	by := map[string]*Fn{}
	get := func(name string) *Fn {
		f := by[name]
		if f == nil {
			f = &Fn{Name: name}
			by[name] = f
		}
		return f
	}
	seen := map[string]bool{}
	for _, s := range stacks {
		if s.Value <= 0 || len(s.Frames) == 0 {
			continue
		}
		clear(seen)
		for _, fr := range s.Frames {
			if !seen[fr] {
				seen[fr] = true
				get(fr).Total += s.Value
			}
		}
		get(s.Frames[len(s.Frames)-1]).Self += s.Value
	}
	out := make([]Fn, 0, len(by))
	for _, f := range by {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Self != out[j].Self {
			return out[i].Self > out[j].Self
		}
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// Count returns the number of nodes of a tree (tests and limits).
func Count(n *Node) int {
	c := 1
	for _, k := range n.Children {
		c += Count(k)
	}
	return c
}
