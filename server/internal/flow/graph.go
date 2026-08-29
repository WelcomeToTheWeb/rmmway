// Package flow is the W5-2 event-driven automation engine: automations are
// composed as DAGs of trigger -> action nodes ("flows") and EXECUTED over
// the NATS event bus. Every hop of every run is a bus event; the database
// only holds the replay-safe state (0006_flows.sql):
//
//	trigger — the entry node: a metric condition (or a synthetic trigger
//	          fired through the API / another event source).
//	script  — dispatch a RunScript to the device's agent (W3-3 capability
//	          token rides the command, same dispatch as operator actions).
//	check   — RE-measure a metric (sample strictly after check_after) and
//	          branch: condition holds -> then-edge, not -> else-edge
//	          ("" = the chain ends, run succeeds).
//	notify  — emit a notification through the Notifier seam (log now;
//	          W6-2's NATS/webhook notifier plugs into the same interface)
//	          and continue.
//
// The engine itself is a thin event consumer: it subscribes to the bus,
// applies one idempotent transition per event, and publishes the next hop.
// A flow therefore "runs over NATS" in the strongest sense — take the bus
// out and the chain stops.
package flow

import (
	"fmt"
	"strings"
	"time"
)

// Node kinds (flow graph node "kind" field).
const (
	KindTrigger = "trigger"
	KindScript  = "script"
	KindCheck   = "check"
	KindNotify  = "notify"
)

// Node is one vertex of a flow DAG. Fields apply per kind:
//
//	trigger: Metric, Source, Op, Threshold
//	script:  Lang, Script, TimeoutS
//	check:   Metric, Source, Op, Threshold, Then, Else
//	notify:  Message
//
// Edges: linear nodes (trigger, script, notify) use Next; check uses
// Then/Else. An empty edge target means "the chain ends here" (allowed
// only on check-Else and on the last node of any kind).
type Node struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Name is the operator-facing label rendered by the visual composer.
	Name string `json:"name,omitempty"`

	// trigger / check condition.
	Metric    string  `json:"metric,omitempty"`
	Source    string  `json:"source,omitempty"`
	Op        string  `json:"op,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`

	// script.
	Lang     string `json:"lang,omitempty"`
	Script   string `json:"script,omitempty"`
	TimeoutS int    `json:"timeout_s,omitempty"`

	// check branch.
	Then string `json:"then,omitempty"`
	Else string `json:"else,omitempty"`

	// notify.
	Message string `json:"message,omitempty"`

	// linear edge.
	Next string `json:"next,omitempty"`
}

// Graph is a flow's DAG: the node list (order is irrelevant; edges are
// explicit). The single trigger node is the entry.
type Graph struct {
	Nodes []Node `json:"nodes"`
}

// Flow is a stored flow (row in flows): a named, versioned DAG.
type Flow struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Graph       Graph     `json:"graph"`
	CooldownS   int       `json:"cooldown_seconds"`
	Enabled     bool      `json:"enabled"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// Node returns the node with id, or nil.
func (g *Graph) Node(id string) *Node {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// Trigger returns the flow's entry node.
func (g *Graph) Trigger() *Node {
	for i := range g.Nodes {
		if g.Nodes[i].Kind == KindTrigger {
			return &g.Nodes[i]
		}
	}
	return nil
}

// OutEdges returns the node's outgoing edge targets (next / then+else).
func (n *Node) OutEdges() []string {
	switch n.Kind {
	case KindCheck:
		return []string{n.Then, n.Else}
	default:
		return []string{n.Next}
	}
}

// EvalOp evaluates one comparison. Supported ops: > >= == < <=.
func EvalOp(op string, val, threshold float64) (bool, error) {
	switch op {
	case ">":
		return val > threshold, nil
	case ">=":
		return val >= threshold, nil
	case "==":
		return val == threshold, nil
	case "<":
		return val < threshold, nil
	case "<=":
		return val <= threshold, nil
	default:
		return false, fmt.Errorf("unsupported comparison %q (want >, >=, ==, <, <=)", op)
	}
}

// Holds reports whether a measurement satisfies the node's condition.
func (n *Node) Holds(v float64) bool {
	ok, err := EvalOp(n.Op, v, n.Threshold)
	if err != nil {
		return false
	}
	return ok
}

// DescribeCondition renders "metric OP threshold" for logs / audit rows.
func (n *Node) DescribeCondition() string {
	return strings.TrimSpace(n.Metric+" "+n.Op) + fmt.Sprintf(" %g", n.Threshold)
}

// Validate checks a graph is a well-formed flow DAG. It returns nil when
// the graph may be stored, or an error naming the first problem:
//
//   - non-empty, unique, non-empty node ids
//   - exactly one trigger (the entry)
//   - every node kind known, kind-specific fields present
//   - every edge target exists (no dangling next/then/else)
//   - no cycles (a flow is a DAG; the engine executes one hop per event)
func (g *Graph) Validate() error {
	if len(g.Nodes) == 0 {
		return fmt.Errorf("flow has no nodes (a trigger node is required)")
	}
	// Pass 1: unique, non-empty ids + trigger count.
	seen := map[string]bool{}
	triggers := 0
	for i := range g.Nodes {
		n := &g.Nodes[i]
		if strings.TrimSpace(n.ID) == "" {
			return fmt.Errorf("node %d has an empty id", i)
		}
		if seen[n.ID] {
			return fmt.Errorf("duplicate node id %q", n.ID)
		}
		seen[n.ID] = true
		if n.Kind == KindTrigger {
			triggers++
		}
	}
	// Pass 2: kind-specific fields + edge targets (all ids are known now,
	// so forward references are fine).
	for i := range g.Nodes {
		n := &g.Nodes[i]
		switch n.Kind {
		case KindTrigger:
			if err := checkCondition(n); err != nil {
				return fmt.Errorf("node %q: %v", n.ID, err)
			}
		case KindScript:
			if n.Lang == "" || strings.TrimSpace(n.Script) == "" {
				return fmt.Errorf("script node %q needs lang + script", n.ID)
			}
			switch n.Lang {
			case "sh", "powershell", "python":
			default:
				return fmt.Errorf("script node %q: unsupported lang %q (want sh|powershell|python)", n.ID, n.Lang)
			}
			if n.TimeoutS < 0 || n.TimeoutS > 86400 {
				return fmt.Errorf("script node %q: timeout_s out of range (0..86400)", n.ID)
			}
		case KindCheck:
			if err := checkCondition(n); err != nil {
				return fmt.Errorf("node %q: %v", n.ID, err)
			}
			if n.TimeoutS < 0 || n.TimeoutS > 86400 {
				return fmt.Errorf("check node %q: timeout_s out of range (0..86400)", n.ID)
			}
			if n.Then != "" && !seen[n.Then] {
				return fmt.Errorf("check node %q: unknown then-target %q", n.ID, n.Then)
			}
			if n.Else != "" && !seen[n.Else] {
				return fmt.Errorf("check node %q: unknown else-target %q", n.ID, n.Else)
			}
		case KindNotify:
		default:
			return fmt.Errorf("node %q: unknown kind %q (want trigger|script|check|notify)", n.ID, n.Kind)
		}
		if n.Kind != KindCheck && n.Next != "" && !seen[n.Next] {
			return fmt.Errorf("node %q: unknown next-target %q", n.ID, n.Next)
		}
	}
	if triggers != 1 {
		return fmt.Errorf("flow has %d trigger nodes, want exactly 1", triggers)
	}
	// Cycles: iterative DFS over the explicit edges (white/grey/black).
	color := map[string]int{} // 0 white, 1 grey, 2 black
	var visit func(id string) error
	visit = func(id string) error {
		color[id] = 1
		for _, t := range g.Node(id).OutEdges() {
			if t == "" {
				continue
			}
			if color[t] == 1 {
				return fmt.Errorf("cycle in flow graph through node %q", t)
			}
			if color[t] == 0 {
				if err := visit(t); err != nil {
					return err
				}
			}
		}
		color[id] = 2
		return nil
	}
	if err := visit(g.Trigger().ID); err != nil {
		return err
	}
	// Every node must be reachable from the trigger (unreachable nodes can
	// never execute — a compose-time mistake, not a runtime surprise).
	for i := range g.Nodes {
		if color[g.Nodes[i].ID] == 0 {
			return fmt.Errorf("node %q is unreachable from the trigger", g.Nodes[i].ID)
		}
	}
	return nil
}

func checkCondition(n *Node) error {
	if n.Metric == "" {
		return fmt.Errorf("%s node needs a metric", n.Kind)
	}
	if _, err := EvalOp(n.Op, 0, 0); err != nil {
		return fmt.Errorf("bad op: %v", err)
	}
	return nil
}
