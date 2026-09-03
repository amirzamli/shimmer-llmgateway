package plugins

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// toolBody builds a chat-completions request body around one tool definition.
func toolBody(parameters string) string {
	return `{"model":"m","tools":[{"type":"function","function":{"name":"t","description":"d","parameters":` + parameters + `}}]}`
}

func filterOnce(t *testing.T, body string) string {
	t.Helper()
	p := NewSanitizeTools()
	req := &Request{Body: json.RawMessage(body)}
	if err := p.FilterRequest(context.Background(), req); err != nil {
		t.Fatalf("FilterRequest: %v", err)
	}
	return string(req.Body)
}

func TestSanitizeToolsStripsOneOfWhenEnumPresent(t *testing.T) {
	params := `{"type":"object","properties":{"action":{"type":"string","enum":["a","b"],"oneOf":[{"const":"a","description":"Do A"},{"const":"b","description":"Do B"}]}},"required":["action"]}`
	out := filterOnce(t, toolBody(params))

	var body struct {
		Tools []struct {
			Function struct {
				Parameters struct {
					Properties struct {
						Action map[string]any `json:"action"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		t.Fatalf("output is not JSON: %v (%s)", err, out)
	}
	action := body.Tools[0].Function.Parameters.Properties.Action
	if _, has := action["oneOf"]; has {
		t.Errorf("oneOf survived: %v", action)
	}
	enum, ok := action["enum"].([]any)
	if !ok || len(enum) != 2 {
		t.Errorf("enum = %v, want [a b]", action["enum"])
	}
	if got := body.Tools[0].Function.Parameters.Required; len(got) != 1 || got[0] != "action" {
		t.Errorf("required = %v, want [action]", got)
	}
}

func TestSanitizeToolsStripsAnyOfWhenEnumPresent(t *testing.T) {
	params := `{"type":"object","properties":{"x":{"enum":[1,2],"anyOf":[{"type":"integer"},{"type":"number"}]}}}`
	out := filterOnce(t, toolBody(params))
	if strings.Contains(out, "anyOf") {
		t.Errorf("anyOf survived with enum present: %s", out)
	}
}

func TestSanitizeToolsKeepsCombinatorWithoutEnum(t *testing.T) {
	params := `{"type":"object","properties":{"x":{"oneOf":[{"type":"string"},{"type":"integer"}]},"y":{"anyOf":[{"type":"null"}]}}}`
	out := filterOnce(t, toolBody(params))
	if !strings.Contains(out, "oneOf") || !strings.Contains(out, "anyOf") {
		t.Errorf("combinators without enum must be preserved: %s", out)
	}
}

func TestSanitizeToolsEmptyEnumKeepsCombinator(t *testing.T) {
	params := `{"type":"object","properties":{"x":{"enum":[],"oneOf":[{"type":"string"}]}}}`
	out := filterOnce(t, toolBody(params))
	if !strings.Contains(out, "oneOf") {
		t.Errorf("oneOf must survive an empty enum: %s", out)
	}
}

func TestSanitizeToolsSanitizesNestedNodes(t *testing.T) {
	params := `{"type":"object","$defs":{"inner":{"enum":["x"],"oneOf":[{"const":"x"}]}},"properties":{"list":{"type":"array","items":{"enum":[1],"oneOf":[{"type":"integer"}]}}}}`
	out := filterOnce(t, toolBody(params))
	if strings.Contains(out, "oneOf") {
		t.Errorf("nested oneOf survived: %s", out)
	}
	if !strings.Contains(out, `"enum"`) {
		t.Errorf("enums must survive: %s", out)
	}
}

func TestSanitizeToolsNoChangesPassesThroughByteForByte(t *testing.T) {
	for _, params := range []string{
		`{"type":"object","properties":{"x":{"type":"string"}}}`,
		`{"type":"object","properties":{"x":{"oneOf":[{"type":"string"}]}}}`,
	} {
		in := toolBody(params)
		if out := filterOnce(t, in); out != in {
			t.Errorf("unchanged body was re-serialized:\n in: %s\nout: %s", in, out)
		}
	}
}

func TestSanitizeToolsOnlyChangedToolRewritten(t *testing.T) {
	body := `{"model":"m","tools":[` +
		`{"type":"function","function":{"name":"keep","parameters":{"type":"object","properties":{"p":{"oneOf":[{"type":"string"}]}}}}},` +
		`{"type":"function","function":{"name":"fix","parameters":{"type":"object","properties":{"q":{"enum":["a"],"oneOf":[{"const":"a"}]}}}}}` +
		`]}`
	out := filterOnce(t, body)
	if !strings.Contains(out, "oneOf") {
		t.Errorf("untouched tool's oneOf must survive: %s", out)
	}
	var parsed struct {
		Tools []struct {
			Function struct {
				Name       string `json:"name"`
				Parameters struct {
					Properties map[string]map[string]any `json:"properties"`
				} `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if _, has := parsed.Tools[0].Function.Parameters.Properties["p"]["oneOf"]; !has {
		t.Error("first tool's oneOf was removed")
	}
	if _, has := parsed.Tools[1].Function.Parameters.Properties["q"]["oneOf"]; has {
		t.Error("second tool's oneOf survived despite enum")
	}
}

func TestSanitizeToolsBodiesWithoutToolsUntouched(t *testing.T) {
	for _, body := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","tools":null}`,
		`{"model":"m","tools":{}}`,
		`{"model":"m","messages":[{"role":"user","content":"{\"enum\":[\"a\"],\"oneOf\":[]}"}]}`,
	} {
		if out := filterOnce(t, body); out != body {
			t.Errorf("body without usable tools changed:\n in: %s\nout: %s", body, out)
		}
	}
}

func TestSanitizeToolsNonJSONAndGarbagePassThrough(t *testing.T) {
	for _, body := range []string{`not json`, ``, `[1,2,3]`} {
		if out := filterOnce(t, body); out != body {
			t.Errorf("non-object body changed: %q -> %q", body, out)
		}
	}
}

func TestSanitizeToolsResponseSideNoOp(t *testing.T) {
	p := NewSanitizeTools()
	resp := &Response{Body: json.RawMessage(`{"choices":[{"message":{"content":"{\"enum\":[\"a\"],\"oneOf\":[]}"}}]}`)}
	if err := p.FilterResponse(context.Background(), resp); err != nil {
		t.Fatalf("FilterResponse: %v", err)
	}
	want := `{"choices":[{"message":{"content":"{\"enum\":[\"a\"],\"oneOf\":[]}"}}]}`
	if string(resp.Body) != want {
		t.Errorf("response body changed: %s", resp.Body)
	}
}

func TestSanitizeToolsRegistered(t *testing.T) {
	p, err := Build("sanitize_tools", Options{})
	if err != nil {
		t.Fatalf("Build(sanitize_tools): %v", err)
	}
	if p.Name() != "sanitize_tools" {
		t.Errorf("Name() = %q, want sanitize_tools", p.Name())
	}
}
