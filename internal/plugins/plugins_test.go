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
	if !IsKnown("redact") {
		t.Fatal("redact is not registered")
	}
	known := Known()
	if len(known) != 1 || known[0] != "redact" {
		t.Errorf("Known() = %v, want [redact]", known)
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
