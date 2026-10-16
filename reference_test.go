package main

import (
	"context"
	"encoding/json"
	"github.com/maximhq/bifrost/core/schemas"
	"os"
	"testing"
	"time"
)

func TestChatMockReferenceMessagesAndTools(t *testing.T) {
	data, err := os.ReadFile("testdata/chatmock_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Messages []struct {
			Name     string
			Messages []schemas.ChatMessage
			Expected []map[string]any
		}
		Tools struct {
			Input    []schemas.ChatTool
			Expected []map[string]any
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Messages {
		t.Run(c.Name, func(t *testing.T) {
			got := messagesToResponsesInput(reorderSystemMessage(c.Messages))
			if canonicalJSON(got) != canonicalJSON(c.Expected) {
				t.Fatalf("got %s; ChatMock %s", canonicalJSON(got), canonicalJSON(c.Expected))
			}
		})
	}
	if got := toolsToResponses(fixture.Tools.Input); canonicalJSON(got) != canonicalJSON(fixture.Tools.Expected) {
		t.Fatalf("tools: got %s; ChatMock %s", canonicalJSON(got), canonicalJSON(fixture.Tools.Expected))
	}
}

func TestChatMockReferenceStreams(t *testing.T) {
	data, err := os.ReadFile("testdata/chatmock_reference.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Streams []struct {
			Mode     string
			Events   []json.RawMessage
			Expected []map[string]any
		}
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	previous := cfg.ReasoningCompat
	t.Cleanup(func() { cfg.ReasoningCompat = previous })
	for _, c := range fixture.Streams {
		t.Run(c.Mode, func(t *testing.T) {
			cfg.ReasoningCompat = c.Mode
			events := make([]string, len(c.Events))
			for i, e := range c.Events {
				events[i] = string(e)
			}
			ch := make(chan *schemas.BifrostStreamChunk, 64)
			streamChatChunks(schemas.NewBifrostContext(context.Background(), time.Time{}), ch, testCodexStream(events...), "gpt-5.4", 0, true)
			var got []any
			for chunk := range ch {
				if chunk.BifrostError != nil {
					t.Fatalf("stream error: %v", chunk.BifrostError)
				}
				got = append(got, chunk.BifrostChatResponse.ExtraFields.RawResponse)
			}
			if canonicalJSON(got) != canonicalJSON(c.Expected) {
				t.Fatalf("wire mismatch\ngot: %s\nChatMock: %s", canonicalJSON(got), canonicalJSON(c.Expected))
			}
		})
	}
}
