package routing

import "sort"

// FindCycle returns the nodes of one cycle in the directed graph, starting and
// ending at the same node (a self-loop is [a a]), or nil when the graph is
// acyclic. Traversal order is sorted, so the reported cycle is deterministic.
func FindCycle(edges map[string][]string) []string {
	const (
		unseen = iota
		active
		done
	)
	state := map[string]int{}
	var stack []string
	var found []string
	var visit func(node string) bool
	visit = func(node string) bool {
		state[node] = active
		stack = append(stack, node)
		next := append([]string(nil), edges[node]...)
		sort.Strings(next)
		for _, to := range next {
			switch state[to] {
			case active:
				i := len(stack) - 1
				for stack[i] != to {
					i--
				}
				found = append(append([]string(nil), stack[i:]...), to)
				return true
			case unseen:
				if visit(to) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = done
		return false
	}
	nodes := make([]string, 0, len(edges))
	for n := range edges {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	for _, n := range nodes {
		if state[n] == unseen && visit(n) {
			return found
		}
	}
	return nil
}
