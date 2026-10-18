package routing

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func locals() []Source {
	return []Source{{"primary", Local}, {"secondary", Local}, {"upstream", Remote}, {"mirror", Replica}}
}
func router(t *testing.T, rules []Rule, defaultTarget string) *Router {
	t.Helper()
	r, err := Compile(Config{Sources: locals(), Rules: rules, DefaultTarget: defaultTarget})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func resolve(t *testing.T, r *Router, q Request) Decision {
	t.Helper()
	d, err := r.Resolve(q)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPriorityDenyAndDefault(t *testing.T) {
	r := router(t, []Rule{
		{ID: "low", Priority: 1, Selector: Selector{Namespace: "team"}, Target: "primary"},
		{ID: "specific", Priority: 10, Selector: Selector{Namespace: "team", Package: "image", Principal: "publisher"}, Target: "secondary"},
		{ID: "block", Priority: -100, Selector: Selector{PathPrefix: "/restricted"}, Deny: true},
	}, "primary")
	d := resolve(t, r, Request{Namespace: "team", Package: "image", Principal: "publisher", Path: "/normal/file"})
	if !d.Allowed || d.TargetID != "secondary" || d.RuleID != "specific" || d.Reason != SelectedRule {
		t.Fatalf("unexpected selection: %+v", d)
	}
	if d.Trace[1].RuleID != "low" || d.Trace[1].Reason != LowerPriority || d.Trace[2].Reason != PathMismatch {
		t.Fatalf("bad explanations: %+v", d.Trace)
	}
	d = resolve(t, r, Request{Namespace: "team", Package: "image", Principal: "publisher", Path: "/restricted/file"})
	if d.Allowed || d.TargetID != "" || d.RuleID != "block" || d.Reason != DeniedByRule {
		t.Fatalf("deny did not override: %+v", d)
	}
	if d.Trace[0].Reason != DenyOverride || d.Trace[1].Reason != DenyOverride || !d.Trace[2].Selected {
		t.Fatal(d.Trace)
	}
	d = resolve(t, r, Request{Namespace: "elsewhere"})
	if !d.Allowed || d.TargetID != "primary" || d.Reason != DefaultDestination {
		t.Fatal(d)
	}
	d = resolve(t, router(t, nil, ""), Request{})
	if d.Allowed || d.Reason != NoDestination || d.TargetID != "" {
		t.Fatal(d)
	}
}

func TestEqualPriorityIntersectionAcrossDimensions(t *testing.T) {
	cases := []struct {
		name     string
		a, b     Selector
		conflict bool
	}{
		{"wildcard", Selector{}, Selector{Namespace: "n"}, true},
		{"cross dimensions", Selector{Namespace: "n"}, Selector{Package: "p", Principal: "u", PathPrefix: "/path"}, true},
		{"different namespace", Selector{Namespace: "a", PathPrefix: "/same"}, Selector{Namespace: "b", PathPrefix: "/same"}, false},
		{"different package", Selector{Namespace: "a", Package: "p"}, Selector{Namespace: "a", Package: "q"}, false},
		{"different principal", Selector{Principal: "alice"}, Selector{Principal: "bob"}, false},
		{"prefix nesting", Selector{PathPrefix: "/a"}, Selector{PathPrefix: "/a/b"}, true},
		{"root prefix", Selector{PathPrefix: "/"}, Selector{PathPrefix: "/a/b"}, true},
		{"segment boundary", Selector{PathPrefix: "/a"}, Selector{PathPrefix: "/ab"}, false},
		{"divergent path", Selector{PathPrefix: "/a/b"}, Selector{PathPrefix: "/a/c"}, false},
		{"normalized prefixes", Selector{PathPrefix: "/a%20b/"}, Selector{PathPrefix: "/a b/c"}, true},
		{"nested prefix but disjoint principal", Selector{PathPrefix: "/a", Principal: "alice"}, Selector{PathPrefix: "/a/b", Principal: "bob"}, false},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Compile(Config{Sources: locals(), Rules: []Rule{{ID: "a", Priority: 3, Selector: tt.a, Target: "primary"}, {ID: "b", Priority: 3, Selector: tt.b, Target: "secondary"}}})
			var configError *ConfigError
			if tt.conflict {
				if !errors.As(err, &configError) || configError.Code != "ambiguous_equal_priority" {
					t.Fatalf("expected conflict, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStableSameTargetTieAndDenyOverlap(t *testing.T) {
	rules := []Rule{{ID: "z", Priority: 7, Target: "primary"}, {ID: "a", Priority: 7, Target: "primary"}}
	first := resolve(t, router(t, rules, ""), Request{})
	rules[0], rules[1] = rules[1], rules[0]
	second := resolve(t, router(t, rules, ""), Request{})
	if !reflect.DeepEqual(first, second) || first.RuleID != "a" || first.Trace[1].Reason != StableTieBreak {
		t.Fatalf("unstable tie: %+v %+v", first, second)
	}
	rules = append(rules, Rule{ID: "deny", Priority: 7, Deny: true})
	if decision := resolve(t, router(t, rules, ""), Request{}); decision.Allowed || decision.RuleID != "deny" {
		t.Fatal(decision)
	}
}

func TestInvalidConfigurationsFailBeforeResolution(t *testing.T) {
	tests := []Config{
		{Sources: locals(), DefaultTarget: "upstream"},
		{Sources: locals(), DefaultTarget: "mirror"},
		{Sources: locals(), DefaultTarget: "missing"},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "upstream"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "mirror"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "missing"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Deny: true, Target: "primary"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "primary"}, {ID: "route", Target: "primary"}}},
		{Sources: []Source{{"", Local}}},
		{Sources: []Source{{"same", Local}, {"same", Remote}}},
		{Sources: []Source{{"source", SourceKind("unknown")}}},
		{Sources: locals(), Rules: []Rule{{Target: "primary"}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "primary", Selector: Selector{Namespace: " padded "}}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "primary", Selector: Selector{Principal: strings.Repeat("x", MaxSelectorBytes+1)}}}},
		{Sources: locals(), Rules: []Rule{{ID: "route", Target: "primary", Selector: Selector{PathPrefix: "/a/../b"}}}},
		{Sources: locals(), Rules: make([]Rule, MaxRules+1)},
		{Sources: make([]Source, MaxSources+1)},
	}
	for i, config := range tests {
		if _, err := Compile(config); err == nil {
			t.Fatalf("invalid configuration %d accepted", i)
		}
	}
}

func TestNormalizeSafePaths(t *testing.T) {
	for input, want := range map[string]string{"/": "/", "/team/a": "/team/a", "/team/a/": "/team/a", "/a%20b": "/a b", "/caf%C3%A9": "/café"} {
		got, err := NormalizePath(input)
		if err != nil || got != want {
			t.Fatalf("normalize %q = %q, %v", input, got, err)
		}
	}
	for _, input := range []string{"", "relative", "//", "/a//b", "/a//", "/./a", "/a/..", "/a/%2E%2e", "/a%2fb", "/a%2Fb", "/a%5Cb", "/a\\b", "/a%252fb", "/a%", "/a%00b", "/a\nb", "/a?b", "/a#b", "/%3f", "/%ff", "/\xff", strings.Repeat("x", MaxPathBytes+1)} {
		if _, err := NormalizePath(input); !errors.Is(err, ErrInvalidPath) {
			t.Fatalf("unsafe path %q accepted: %v", input, err)
		}
	}
}

func TestSelectorConjunctionAndAbsentFields(t *testing.T) {
	r := router(t, []Rule{{ID: "route", Target: "primary", Selector: Selector{Namespace: "n", Package: "p", Principal: "u", PathPrefix: "/a"}}}, "")
	for _, request := range []Request{{Namespace: "n", Package: "p", Principal: "u", Path: "/a"}, {Namespace: "n", Package: "p", Principal: "u", Path: "/a/child"}} {
		if !resolve(t, r, request).Allowed {
			t.Fatal("matching request rejected")
		}
	}
	cases := []struct {
		request Request
		reason  Reason
	}{
		{Request{Package: "p", Principal: "u", Path: "/a"}, NamespaceMismatch},
		{Request{Namespace: "n", Principal: "u", Path: "/a"}, PackageMismatch},
		{Request{Namespace: "n", Package: "p", Path: "/a"}, PrincipalMismatch},
		{Request{Namespace: "n", Package: "p", Principal: "u"}, PathMissing},
		{Request{Namespace: "n", Package: "p", Principal: "u", Path: "/ab"}, PathMismatch},
	}
	for _, tt := range cases {
		d := resolve(t, r, tt.request)
		if d.Allowed || d.Trace[0].Reason != tt.reason {
			t.Fatalf("unexpected match: %+v", d)
		}
	}
	root := router(t, []Rule{{ID: "root", Target: "primary", Selector: Selector{PathPrefix: "/"}}}, "")
	if resolve(t, root, Request{}).Allowed || !resolve(t, root, Request{Path: "/"}).Allowed {
		t.Fatal("root/absent path semantics violated")
	}
}

func TestNoInputValuesInExplanation(t *testing.T) {
	secret := "private-principal-value"
	r := router(t, []Rule{{ID: "route", Target: "primary", Selector: Selector{Principal: "allowed-principal"}}}, "")
	decision := resolve(t, r, Request{Principal: secret, Namespace: "private-namespace"})
	encoded, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{secret, "private-namespace", "allowed-principal"} {
		if strings.Contains(string(encoded), value) {
			t.Fatal("selector value leaked in explanation")
		}
	}
	decision, err = r.Resolve(Request{Principal: secret, Path: "/../invalid"})
	if !errors.Is(err, ErrInvalidRequest) || decision.Reason != InvalidInput || len(decision.Trace) != 0 {
		t.Fatal("invalid input did not fail closed")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "/../invalid") {
		t.Fatal("request input leaked in error")
	}
}

func TestRouterIsImmutableAndConcurrent(t *testing.T) {
	rules := []Rule{{ID: "route", Target: "primary", Selector: Selector{Namespace: "team"}}}
	r := router(t, rules, "")
	rules[0].Target = "secondary"
	rules[0].Selector.Namespace = "other"
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				decision, err := r.Resolve(Request{Namespace: "team"})
				if err != nil || !decision.Allowed || decision.TargetID != "primary" {
					t.Errorf("mutable policy: %+v %v", decision, err)
					return
				}
				decision.Trace[0].Reason = DeniedByRule
			}
		}()
	}
	wg.Wait()
	if resolve(t, r, Request{Namespace: "team"}).Trace[0].Reason != SelectedRule {
		t.Fatal("trace mutation leaked across calls")
	}
}

// Exhaustive finite witnesses verify intersections across all four dimensions,
// including selectors that constrain different dimensions rather than only one.
func TestIntersectionMatchesExistenceOfCommonRequest(t *testing.T) {
	selectors := []Selector{{}, {Namespace: "n"}, {Namespace: "other"}, {Package: "p"}, {Principal: "u"}, {Principal: "v"}, {PathPrefix: "/"}, {PathPrefix: "/a"}, {PathPrefix: "/a/b"}, {PathPrefix: "/ab"}, {Namespace: "n", Package: "p", PathPrefix: "/a", Principal: "u"}, {Namespace: "other", Package: "p", PathPrefix: "/a/b", Principal: "u"}}
	for _, a := range selectors {
		for _, b := range selectors {
			witness := false
			for _, namespace := range []string{"", "n", "other"} {
				for _, pkg := range []string{"", "p"} {
					for _, principal := range []string{"", "u", "v"} {
						for _, path := range []string{"", "/", "/a", "/a/b", "/ab"} {
							request := Request{namespace, pkg, path, principal}
							if mismatch(a, request) == "" && mismatch(b, request) == "" {
								witness = true
							}
						}
					}
				}
			}
			if intersects(a, b) != witness {
				t.Fatalf("intersection mismatch: %+v %+v, witness=%v", a, b, witness)
			}
		}
	}
}
