"""Generate offline parity fixtures from an unmodified local ChatMock checkout.

Only selected pure functions are compiled from its AST. Imports, server startup,
credential loading and network requests never execute. No third-party modules
are needed. Usage: python tools/chatmock_reference.py PATH_TO_CHATMOCK
"""
import ast
import base64
import hashlib
import json
import sys
from pathlib import Path

root = Path(sys.argv[1]).resolve()
utils = root / "chatmock" / "utils.py"
tree = ast.parse(utils.read_text(encoding="utf-8"))
tree.body = [node for node in tree.body if isinstance(node, ast.FunctionDef) and node.name in {
    "convert_chat_messages_to_responses_input", "convert_tools_chat_to_responses", "sse_translate_chat"
}]
tree.body.insert(0, ast.ImportFrom(module="__future__", names=[ast.alias(name="annotations")], level=0))
namespace = {"json": json, "base64": base64}
exec(compile(ast.fix_missing_locations(tree), str(utils), "exec"), namespace)

cases = [
    {"name": "developer", "messages": [{"role": "developer", "content": "Follow this rule"}, {"role": "user", "content": "Hello"}]},
    {"name": "system_order", "messages": [{"role": "user", "content": "Hello"}, {"role": "system", "content": "System rule"}, {"role": "system", "content": "second system"}]},
    {"name": "tool_blocks", "messages": [{"role": "tool", "tool_call_id": "call_1", "content": [{"type": "text", "text": "first"}, {"type": "text", "text": "second"}]}]},
    {"name": "empty_tool_result", "messages": [{"role": "tool", "tool_call_id": "call_1", "content": ""}]},
    {"name": "vision", "messages": [{"role": "user", "content": [{"type": "text", "text": "Describe"}, {"type": "image_url", "image_url": {"url": "data:image/png;base64,/w%3D%3D"}}]}]},
    {"name": "assistant_tools", "messages": [{"role": "assistant", "content": "Checking", "tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{\"q\":\"hello\"}"}}]}]},
]
for case in cases:
    messages = json.loads(json.dumps(case["messages"]))
    for index, message in enumerate(messages):
        if message.get("role") == "system":
            messages.insert(0, {"role": "user", "content": messages.pop(index).get("content", "")})
            break
    case["expected"] = namespace["convert_chat_messages_to_responses_input"](messages)

tools = [{"type": "function", "function": {"name": "lookup", "description": "Find a record", "parameters": {"type": "object", "properties": {"q": {"type": "string"}}}}}, {"type": "function", "function": {"name": "ping"}}]

class FakeUpstream:
    def __init__(self, events): self.events = events
    def iter_lines(self, **_):
        for event in self.events: yield b"data: " + json.dumps(event).encode()
    def close(self): pass

stream_cases = []
for compat in ("think-tags", "o3", "legacy"):
    events = [
        {"type": "response.reasoning_summary_part.added"},
        {"type": "response.reasoning_summary_text.delta", "delta": "first"},
        {"type": "response.reasoning_summary_part.added"},
        {"type": "response.reasoning_summary_text.delta", "delta": "second"},
        {"type": "response.output_text.delta", "delta": "answer"},
        {"type": "response.output_text.done", "text": "answer"},
        {"type": "response.completed", "response": {"id": "response_1", "usage": {"input_tokens": 5, "output_tokens": 3}}},
    ]
    chunks = []
    for encoded in namespace["sse_translate_chat"](FakeUpstream(events), "gpt-5.4", 0, reasoning_compat=compat, include_usage=True):
        text = encoded.decode().removeprefix("data: ").strip()
        if text != "[DONE]": chunks.append(json.loads(text))
    stream_cases.append({"mode": compat, "events": events, "expected": chunks})

fixture = {"reference": "ChatMock 5bd6e77", "source_sha256": hashlib.sha256(utils.read_bytes()).hexdigest(), "messages": cases, "tools": {"input": tools, "expected": namespace["convert_tools_chat_to_responses"](tools)}, "streams": stream_cases}
destination = Path(__file__).resolve().parents[1] / "testdata" / "chatmock_reference.json"
destination.parent.mkdir(exist_ok=True)
destination.write_text(json.dumps(fixture, indent=2) + "\n", encoding="utf-8")
print(f"Generated {len(cases)} message and {len(stream_cases)} stream reference cases: {destination}")
