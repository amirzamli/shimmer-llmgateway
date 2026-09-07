package plugins

import (
	"context"
	"strings"
	"testing"
)

// spanPlugin appends its name to log when its filter runs, proving chain order.
type spanPlugin struct {
	name string
	log  *[]string
}

func (p *spanPlugin) Name() string { return p.name }

func (p *spanPlugin) FilterRequest(ctx context.Context, req *Request) error {
	*p.log = append(*p.log, p.name+":req")
	return nil
}

func (p *spanPlugin) FilterResponse(ctx context.Context, resp *Response) error {
	*p.log = append(*p.log, p.name+":resp")
	return nil
}

func TestChainOrdering(t *testing.T) {
	// §4.5 ordering: request plugins in config order before forward, response
	// plugins in config order post-reassembly.
	var log []string
	chain := NewChain()
	chain.AddRequest(&spanPlugin{name: "a", log: &log})
	chain.AddRequest(&spanPlugin{name: "b", log: &log})
	chain.AddResponse(&spanPlugin{name: "c", log: &log})
	chain.AddResponse(&spanPlugin{name: "d", log: &log})

	if !chain.HasRequest() || !chain.HasResponse() {
		t.Fatal("HasRequest/HasResponse should be true for a populated chain")
	}
	if err := chain.FilterRequest(context.Background(), &Request{}); err != nil {
		t.Fatalf("FilterRequest: %v", err)
	}
	if err := chain.FilterResponse(context.Background(), &Response{}); err != nil {
		t.Fatalf("FilterResponse: %v", err)
	}
	got := strings.Join(log, ",")
	want := "a:req,b:req,c:resp,d:resp"
	if got != want {
		t.Errorf("chain order = %q, want %q", got, want)
	}
}

func TestChainAppliedDedupesUnion(t *testing.T) {
	var log []string
	chain := NewChain()
	chain.AddRequest(&spanPlugin{name: "redact", log: &log})
	chain.AddRequest(&spanPlugin{name: "req-only", log: &log})
	chain.AddResponse(&spanPlugin{name: "redact", log: &log})

	got := chain.Applied()
	want := "redact,req-only"
	if strings.Join(got, ",") != want {
		t.Errorf("Applied() = %v, want [%s]", got, want)
	}
}

func TestEmptyChainHasNoSideAndNilApplied(t *testing.T) {
	chain := NewChain()
	if chain.HasRequest() || chain.HasResponse() {
		t.Error("empty chain should have no sides")
	}
	if chain.Applied() != nil {
		t.Errorf("Applied() = %v, want nil (→ NULL plugins_applied)", chain.Applied())
	}
	if err := chain.FilterRequest(context.Background(), &Request{}); err != nil {
		t.Errorf("FilterRequest on empty chain: %v", err)
	}
	if err := chain.FilterResponse(context.Background(), &Response{}); err != nil {
		t.Errorf("FilterResponse on empty chain: %v", err)
	}
}

// failPlugin returns an error from FilterResponse to prove error propagation.
type failPlugin struct{ name string }

func (p *failPlugin) Name() string { return p.name }
func (p *failPlugin) FilterRequest(ctx context.Context, req *Request) error {
	return nil
}
func (p *failPlugin) FilterResponse(ctx context.Context, resp *Response) error {
	return context.Canceled
}

func TestChainErrorWrapsPluginName(t *testing.T) {
	chain := NewChain()
	chain.AddResponse(&failPlugin{name: "boom"})
	err := chain.FilterResponse(context.Background(), &Response{})
	if err == nil {
		t.Fatal("FilterResponse returned nil, want error")
	}
	if !strings.Contains(err.Error(), `plugin "boom"`) {
		t.Errorf("error %q should name the failing plugin", err)
	}
}

func TestRegistryKnownIncludesRedact(t *testing.T) {
	known := Known()
	want := []string{"fill_reasoning_content", "redact", "sanitize_tools"}
	if strings.Join(known, ",") != strings.Join(want, ",") {
		t.Errorf("Known() = %v, want %v", known, want)
	}
}

func TestIsRequestOnly(t *testing.T) {
	// sanitize_tools repairs client tool schemas, which only exist on the
	// request side; redact filters both sides; spanPlugin declares nothing.
	sanitize, err := Build("sanitize_tools", Options{})
	if err != nil {
		t.Fatalf("Build(sanitize_tools): %v", err)
	}
	if !IsRequestOnly(sanitize) {
		t.Error("sanitize_tools should be request-only")
	}
	redact, err := Build("redact", Options{})
	if err != nil {
		t.Fatalf("Build(redact): %v", err)
	}
	if IsRequestOnly(redact) {
		t.Error("redact should not be request-only (it filters both sides)")
	}
	if IsRequestOnly(&spanPlugin{name: "x"}) {
		t.Error("a plugin without the RequestOnly capability should not be request-only")
	}
}

// TestInfosCoversEveryPlugin pins the UI-facing metadata contract: one Info
// per known plugin, each with a description and a source, and the redact info
// documenting both config fields with the default patterns attached.
func TestInfosCoversEveryPlugin(t *testing.T) {
	infos := Infos()
	if len(infos) != len(Known()) {
		t.Fatalf("Infos() len = %d, want %d (one per known plugin)", len(infos), len(Known()))
	}
	byName := map[string]Info{}
	for _, i := range infos {
		if i.Name == "" || i.Description == "" || i.Source == "" {
			t.Errorf("incomplete Info: %+v", i)
		}
		if i.Kind != "transform" {
			t.Errorf("Info %q: kind = %q, want transform", i.Name, i.Kind)
		}
		byName[i.Name] = i
	}
	redact, ok := byName["redact"]
	if !ok {
		t.Fatal("Infos() missing redact")
	}
	if !redact.Configurable || len(redact.ConfigFields) != 2 {
		t.Fatalf("redact Info = %+v, want configurable with 2 config fields", redact)
	}
	var patterns ConfigField
	for _, f := range redact.ConfigFields {
		if f.Name == "patterns" {
			patterns = f
		}
	}
	if patterns.Name == "" || len(patterns.Defaults) != len(defaultPatterns) {
		t.Errorf("redact patterns field = %+v, want defaults mirroring defaultPatterns", patterns)
	}
}

func TestBuildUnknownPlugin(t *testing.T) {
	_, err := Build("nope", Options{})
	if err == nil {
		t.Fatal("Build(nope) returned no error")
	}
	if !strings.Contains(err.Error(), "unknown plugin") {
		t.Errorf("error %q should mention unknown plugin", err)
	}
}

func TestBuildRedactWithConfig(t *testing.T) {
	p, err := Build("redact", Options{Config: map[string]any{
		"patterns":    []any{`TOKEN[0-9]+`},
		"field_names": []any{"password"},
	}})
	if err != nil {
		t.Fatalf("Build(redact): %v", err)
	}
	if p.Name() != "redact" {
		t.Errorf("Name() = %q, want redact", p.Name())
	}
}
