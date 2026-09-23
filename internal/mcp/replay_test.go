package mcp

import (
	"encoding/json"
	"testing"
)

func replayMessage(role, content string) openAIMessage {
	return openAIMessage{Role: role, Content: json.RawMessage(`"` + content + `"`)}
}

func TestNewMessagesTailAlignsAcrossSystemPromptReplacement(t *testing.T) {
	old := []openAIMessage{
		replayMessage("system", "old instructions"),
		replayMessage("user", "hello"),
		replayMessage("assistant", "hi"),
	}
	cur := []openAIMessage{
		replayMessage("system", "new instructions"),
		replayMessage("user", "hello"),
		replayMessage("assistant", "hi"),
		replayMessage("user", "new turn"),
	}

	fresh := newMessagesTail(old, cur)
	if len(fresh) != 2 || fresh[0].Role != "system" || fresh[1].Role != "user" {
		t.Fatalf("fresh = %+v, want replacement system + new user", fresh)
	}
	if string(fresh[0].Content) != `"new instructions"` || string(fresh[1].Content) != `"new turn"` {
		t.Fatalf("fresh content = %s, %s", fresh[0].Content, fresh[1].Content)
	}
}

func TestNewMessagesTailDoesNotRepeatRenderedAssistantResponse(t *testing.T) {
	firstRequest := []openAIMessage{
		replayMessage("system", "instructions"),
		replayMessage("user", "hello"),
	}
	response := openAIMessage{Role: "assistant", Content: json.RawMessage(`"answer"`)}
	previous := append(append([]openAIMessage{}, firstRequest...), response)
	secondRequest := []openAIMessage{
		replayMessage("system", "instructions"),
		replayMessage("user", "hello"),
		response,
		replayMessage("user", "next"),
	}

	fresh := newMessagesTail(previous, secondRequest)
	if len(fresh) != 1 || fresh[0].Role != "user" || string(fresh[0].Content) != `"next"` {
		t.Fatalf("fresh = %+v, want only next user message", fresh)
	}
}
