package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/maximhq/bifrost/core/schemas"
)

func TestNormalizeChatHTTPRequestPreservesAcceptedMessageAliases(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"tool result id and content", `{"messages":[{"role":"tool","id":"call_1","content":[{"content":"first"},{"text":"","content":"second"}]}]}`, `[{"type":"function_call_output","call_id":"call_1","output":"first\nsecond"}]`},
		{"assistant call id", `{"messages":[{"role":"assistant","tool_calls":[{"call_id":"call_1","function":{"name":"lookup","arguments":"{}"}}]}]}`, `[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]`},
		{"text and image aliases", `{"messages":[{"role":"user","content":[{"type":"text","content":"Describe"},{"type":"image_url","image_url":"https://example.test/image.png"}]}]}`, `[{"type":"message","role":"user","content":[{"type":"input_text","text":"Describe"},{"type":"input_image","image_url":"https://example.test/image.png"}]}]`},
		{"canonical field precedence", `{"messages":[{"role":"tool","tool_call_id":"canonical","id":"alias","content":[{"text":"canonical","content":"alias"}]}]}`, `[{"type":"function_call_output","call_id":"canonical","output":"canonical"}]`},
		{"empty canonical fields use alias", `{"messages":[{"role":"assistant","tool_calls":[{"id":"","call_id":"call_1","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"","id":"call_1","content":[{"text":null,"content":"result"}]}]}`, `[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"result"}]`},
		{"false canonical values use alias", `{"messages":[{"role":"tool","tool_call_id":false,"id":"call_1","content":[{"text":0e0,"content":"zero"},{"text":[],"content":"list"},{"text":{},"content":"object"}]}]}`, `[{"type":"function_call_output","call_id":"call_1","output":"zero\nlist\nobject"}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var wire struct {
				Messages []schemas.ChatMessage `json:"messages"`
			}
			if err := json.Unmarshal(normalizeChatHTTPRequest([]byte(c.body)), &wire); err != nil {
				t.Fatal(err)
			}
			got := messagesToResponsesInput(wire.Messages)
			encoded, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			assertWireJSONEqual(t, encoded, []byte(c.want))
		})
	}
}

func TestNormalizeChatHTTPRequestPromptAndInputFallbacks(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{"prompt":"prompt","input":"input"}`, `{"prompt":"prompt","input":"input","messages":[{"role":"user","content":"prompt"}]}`},
		{`{"messages":null,"input":"input"}`, `{"messages":[{"role":"user","content":"input"}],"input":"input"}`},
		{`{"messages":[],"prompt":"ignored"}`, `{"messages":[],"prompt":"ignored"}`},
		{`{"messages":"invalid","prompt":"ignored"}`, `{"messages":"invalid","prompt":"ignored"}`},
		{`{"prompt":"","input":"ignored"}`, `{"prompt":"","input":"ignored","messages":[{"role":"user","content":""}]}`},
		{`{"prompt":"hello","metadata":{"large":9007199254740993}}`, `{"prompt":"hello","metadata":{"large":9007199254740993},"messages":[{"role":"user","content":"hello"}]}`},
	} {
		got := normalizeChatHTTPRequest([]byte(c.body))
		assertWireJSONEqual(t, got, []byte(c.want))
	}
}

func TestNormalizeChatHTTPRequestRepairsOnlyRecoverableJSON(t *testing.T) {
	body := []byte("{\"messages\":[{\"role\":\"user\",\"content\":\"line\r\nbreak\"}],\"metadata\":{\"number\":9007199254740993}}")
	got := normalizeChatHTTPRequest(body)
	want := `{"messages":[{"role":"user","content":"linebreak"}],"metadata":{"number":9007199254740993}}`
	assertWireJSONEqual(t, got, []byte(want))
	for _, invalid := range []string{`{"messages":`, `[]`, `null`, `{"messages":[]} trailing`} {
		if got := normalizeChatHTTPRequest([]byte(invalid)); !bytes.Equal(got, []byte(invalid)) {
			t.Errorf("changed invalid body %q to %s", invalid, got)
		}
	}
}

func assertWireJSONEqual(t *testing.T, got, want []byte) {
	t.Helper()
	decode := func(data []byte) any {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("invalid JSON %s: %v", data, err)
		}
		return value
	}
	if !reflect.DeepEqual(decode(got), decode(want)) {
		t.Errorf("got %s; want %s", got, want)
	}
}
