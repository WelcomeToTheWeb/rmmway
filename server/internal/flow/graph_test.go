package flow

import "testing"

// doDGraph is the W5-2 definition-of-done chain:
// disk>90% -> free -> if>90% -> notify.
func doDGraph() Graph {
	return Graph{Nodes: []Node{
		{ID: "t", Kind: KindTrigger, Name: "disk > 90%", Metric: "disk.used_percent", Op: ">", Threshold: 90, Next: "free"},
		{ID: "free", Kind: KindScript, Name: "free space", Lang: "sh", Script: "df -h", TimeoutS: 120, Next: "still"},
		{ID: "still", Kind: KindCheck, Name: "if still > 90%", Metric: "disk.used_percent", Op: ">", Threshold: 90, Then: "notify", Else: ""},
		{ID: "notify", Kind: KindNotify, Name: "notify", Message: "disk still full after cleanup"},
	}}
}

func TestDoDGraphValidates(t *testing.T) {
	g := doDGraph()
	if err := g.Validate(); err != nil {
		t.Fatalf("DoD chain should validate, got: %v", err)
	}
	trig := g.Trigger()
	if trig == nil || trig.ID != "t" {
		t.Fatalf("trigger = %+v, want node t", trig)
	}
}

func TestEvalOp(t *testing.T) {
	cases := []struct {
		op       string
		val, thr float64
		want     bool
	}{
		{">", 95, 90, true}, {">", 90, 90, false}, {">", 89, 90, false},
		{">=", 90, 90, true}, {">=", 89, 90, false},
		{"==", 0, 0, true}, {"==", 1, 0, false},
		{"<", 89, 90, true}, {"<", 90, 90, false},
		{"<=", 90, 90, true}, {"<=", 91, 90, false},
	}
	for _, c := range cases {
		got, err := EvalOp(c.op, c.val, c.thr)
		if err != nil {
			t.Fatalf("EvalOp(%q): %v", c.op, err)
		}
		if got != c.want {
			t.Errorf("EvalOp(%q, %v, %v) = %v, want %v", c.op, c.val, c.thr, got, c.want)
		}
	}
	if _, err := EvalOp("~", 1, 1); err == nil {
		t.Errorf("EvalOp(~) should fail")
	}
}

func TestValidateRejects(t *testing.T) {
	mutate := func(f func(*Node)) Graph {
		g := doDGraph()
		for i := range g.Nodes {
			if g.Nodes[i].ID == "free" {
				f(&g.Nodes[i])
			}
		}
		return g
	}
	cases := map[string]Graph{
		"empty":                {},
		"no trigger":           {Nodes: []Node{{ID: "a", Kind: KindScript, Lang: "sh", Script: "x", Next: ""}}},
		"two triggers":         {Nodes: []Node{{ID: "a", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1}, {ID: "b", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1}}},
		"duplicate id":         {Nodes: []Node{{ID: "a", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "a"}, {ID: "a", Kind: KindNotify}}},
		"unknown kind":         {Nodes: []Node{{ID: "a", Kind: "bogus"}}},
		"script no lang":       mutate(func(n *Node) { n.Lang = "" }),
		"script bad lang":      mutate(func(n *Node) { n.Lang = "ruby" }),
		"script no script":     mutate(func(n *Node) { n.Script = "  " }),
		"bad op":               {Nodes: []Node{{ID: "a", Kind: KindTrigger, Metric: "m", Op: "~", Threshold: 1}}},
		"trigger no metric":    {Nodes: []Node{{ID: "a", Kind: KindTrigger, Op: ">", Threshold: 1}}},
		"dangling next":        mutate(func(n *Node) { n.Next = "ghost" }),
		"dangling then":        {Nodes: []Node{{ID: "t", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "c"}, {ID: "c", Kind: KindCheck, Metric: "m", Op: ">", Threshold: 1, Then: "ghost"}}},
		"dangling else":        {Nodes: []Node{{ID: "t", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "c"}, {ID: "c", Kind: KindCheck, Metric: "m", Op: ">", Threshold: 1, Else: "ghost"}}},
		"cycle":                {Nodes: []Node{{ID: "a", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "b"}, {ID: "b", Kind: KindNotify, Next: "a"}}},
		"self cycle":           {Nodes: []Node{{ID: "a", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "a"}}},
		"unreachable":          {Nodes: []Node{{ID: "t", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1}, {ID: "orphan", Kind: KindNotify}}},
		"empty node id":        {Nodes: []Node{{ID: "", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1}}},
		"script timeout range": mutate(func(n *Node) { n.TimeoutS = 90000 }),
		"check timeout range ok": func() Graph {
			g := doDGraph()
			for i := range g.Nodes {
				if g.Nodes[i].ID == "still" {
					g.Nodes[i].TimeoutS = 300
				}
			}
			return g
		}(),
	}
	for name, g := range cases {
		err := g.Validate()
		if name == "check timeout range ok" {
			if err != nil {
				t.Errorf("%s: should validate, got %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: should be rejected", name)
		}
	}
}

func TestCheckElseMayEndChain(t *testing.T) {
	// check with else="" (chain ends when the condition is NOT held) is valid.
	g := Graph{Nodes: []Node{
		{ID: "t", Kind: KindTrigger, Metric: "m", Op: ">", Threshold: 1, Next: "c"},
		{ID: "c", Kind: KindCheck, Metric: "m", Op: ">", Threshold: 1, Then: "n", Else: ""},
		{ID: "n", Kind: KindNotify},
	}}
	if err := g.Validate(); err != nil {
		t.Fatalf("else='' should validate: %v", err)
	}
	c := g.Node("c")
	if c.Then != "n" || c.Else != "" {
		t.Fatalf("check edges = (%q, %q), want (n, \"\")", c.Then, c.Else)
	}
	edges := c.OutEdges()
	if len(edges) != 2 {
		t.Fatalf("check OutEdges = %v, want 2", edges)
	}
}

func TestHolds(t *testing.T) {
	n := &Node{Op: ">", Threshold: 90}
	if !n.Holds(95) || n.Holds(90) || n.Holds(89) {
		t.Fatalf("Holds wrong for > 90")
	}
}
