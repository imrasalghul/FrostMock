# FrostMock

Use your existing ChatGPT subscription through [Bifrost](https://github.com/maximhq/bifrost).

FrostMock is a Go adaptation of **[RayBytes/ChatMock](https://github.com/RayBytes/ChatMock)**, adapted to run as a native Bifrost plugin. It brings ChatMock's authentication, request conversion, and response compatibility into Bifrost's gateway and native model catalog.

**FrostMock is intended solely for individual use with your own existing ChatGPT subscription.** It is designed for a personal, self-hosted Bifrost instance.

## Features

- Chat completions, Responses API, and text completions, with streaming support.
- Function calls, tool results, image inputs, and reasoning compatibility modes.
- Browser and headless device login, with automatic token refresh.
- Account model discovery through Bifrost's native catalog.
- Bifrost's existing routing, virtual keys, governance, and logging.

Bifrost serves all inference requests. FrostMock does not run an inference server or expose a separate model endpoint. Browser login uses a temporary loopback listener only for the OAuth callback.

## Requirements

- An existing ChatGPT subscription.
- A Linux or macOS Bifrost host with native Go plugin support.
- Matching Go compiler, platform, shared dependencies, and build flags for Bifrost and the plugin.

The tested integration targets **Bifrost v2.3.0**, **Bifrost core v1.12.0**, and **Go 1.27.0**. It requires a dynamically linked Bifrost build and the two included host patches for native model discovery and auth-route registration. Native Go plugins cannot be loaded on Windows or cross-compiled for a different host.

## Getting started

### Release binaries

The [release workflow](.github/workflows/release.yml) builds Bifrost with both host patches, FrostMock, and the login helper together using Go 1.27.0. Each pair must pass an integration check that loads the plugin into the actual Bifrost executable before it can be released.

| Platform | Bifrost | Plugin |
|---|---|---|
| Linux x64 | `bifrost-linux-amd64` | `frostmock-linux-amd64.so` |
| Linux ARM64 | `bifrost-linux-arm64` | `frostmock-linux-arm64.so` |
| macOS Intel | `bifrost-darwin-amd64` | `frostmock-darwin-amd64.so` |
| macOS Apple Silicon | `bifrost-darwin-arm64` | `frostmock-darwin-arm64.so` |

Download the matching host and plugin from the same [release](https://github.com/imrasalghul/FrostMock/releases). Login helpers are named `frostmock-login-<platform>-<architecture>`. Set the executable bit on Bifrost and the helper with `chmod +x`, and set the plugin path in your configuration to the downloaded `.so`. The provider and plugin identifier is still `chatgpt`.

Linux builds use Ubuntu 24.04 and require compatible glibc libraries. macOS builds use macOS 15 runners and are unsigned. Build from source if you need a different host environment.

Create and publish a GitHub release with your chosen tag, title, and notes. The workflow builds the tagged source and uploads all four platform builds, licenses, and `SHA256SUMS` to that existing release after every build passes. It does not create releases or edit their metadata. Reruns replace assets with the same filenames.

Main-branch pushes and manual runs with an empty `release_tag` only build and test; their binaries are available as workflow artifacts. To rebuild an existing release, run the workflow manually from `main` and enter its tag in `release_tag`. The source is checked out from that tag.

### Build from source

Clone FrostMock and build the plugin and login helper with the toolchain used for your Bifrost host:

```sh
git clone https://github.com/imrasalghul/FrostMock.git
cd FrostMock
CGO_ENABLED=1 make build login-helper \
  BUILD_FLAGS='-trimpath -tags=netgo,osusergo,sqlite_static'
```

This produces `build/chatgpt.so` and `build/chatgpt-login`. The Bifrost provider and plugin identifier remains `chatgpt`.

1. Prepare Bifrost using the steps below.
2. Copy `chatgpt.so` to the Bifrost host.
3. Merge [config.example.json](config.example.json) into your Bifrost configuration, adjusting the plugin path.
4. Log in using the same OS user and credential directory as the Bifrost service.
5. Start Bifrost and select a model from its native catalog.

Keep the plugin's `post_builtin` placement so Bifrost's governance and logging hooks run before FrostMock handles inference.

### Prepare Bifrost

The tested Bifrost release binary was statically linked and could not load native Go plugins. Build a dynamic host using Go 1.27.0, a C compiler, and the Node/npm versions required by Bifrost. Starting from the FrostMock checkout:

```sh
export FROSTMOCK_DIR="$(pwd)"
export CGO_ENABLED=1 GOWORK=off
git clone https://github.com/maximhq/bifrost.git ../bifrost
cd ../bifrost
git checkout 9237ec06672406e437b3cb33fc793ffd0403b3ae
git apply "$FROSTMOCK_DIR/patches/bifrost-v2.3.0-native-plugin-catalog.patch"
git apply "$FROSTMOCK_DIR/patches/bifrost-v2.3.0-plugin-auth-routes.patch"
cd ui
npm ci
npm run build
cd ../transports/bifrost-http
go build -trimpath -tags=netgo,osusergo,sqlite_static \
  -ldflags='-w -s -X main.Version=v2.3.0' -o bifrost-http .
```

The catalog patch connects background discovery to the provider plugin while preserving native model policies. The auth-route patch registers the account-management routes behind Bifrost's dashboard authentication. Without it, `/chatgpt/status` falls through to the dashboard's not-found page.

The plugin and host must match the exact compiler version, shared dependencies, and build flags. Recheck both patches when upgrading Bifrost. The Dockerfile can also build Linux plugin artifacts; set `GO_VERSION` and `BUILD_FLAGS` to match your host.

## Login

For a headless server:

```sh
./build/chatgpt-login login --headless
```

Open the printed URL in your browser, sign into your ChatGPT account, and enter the code. The helper saves the tokens and exits. No callback port is needed.

For browser login on your own machine:

```sh
./build/chatgpt-login login
```

Browser login opens a temporary callback listener on `127.0.0.1:1455`. Use `--no-browser` to print the URL without opening it, or `--device` as an alias for `--headless`.

Credentials are stored in `~/.chatgpt-local/auth.json`. Set `CHATGPT_LOCAL_HOME` to use a different directory, and use the same value for the helper and Bifrost. Existing Codex credentials can be read as a fallback; FrostMock writes and removes only its own credentials. Tokens refresh automatically as needed.

### Account management

With the auth-route patch installed, these routes use Bifrost's dashboard authentication. A signed-in dashboard browser can open `/chatgpt/status` to view JSON. An inference virtual key alone does not grant account-management access.

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/chatgpt/login` | Start device login and return its URL and code |
| `GET` | `/chatgpt/status` | Check authentication and login progress |
| `POST` | `/chatgpt/refresh` | Request a token refresh |
| `POST` | `/chatgpt/logout` | Cancel pending login and remove FrostMock-owned credentials |

After gateway logout, use `/chatgpt/login` to reconnect. If you use the separate CLI helper instead, restart Bifrost afterward to clear its logged-out state. Each plugin instance uses one ChatGPT account.

## Usage

Use Bifrost's standard APIs and credentials. Replace `<model-id>` with a model listed for the `chatgpt` provider:

```sh
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "x-bf-vk: $BIFROST_VIRTUAL_KEY" \
  -d '{
    "model": "chatgpt/<model-id>",
    "messages": [{"role": "user", "content": "Hello!"}],
    "stream": true
  }'
```

Native Responses and text completions use Bifrost's standard `/v1/responses` and `/v1/completions` routes. Model availability depends on the signed-in account. Keep `list_models` enabled in the provider configuration to populate Bifrost's native catalog.

## Configuration

Options belong in the plugin's `config` object. See [config.example.json](config.example.json) for a complete example.

| Option | Default | Purpose |
|---|---|---|
| `reasoning_effort` | `medium` | Default reasoning effort |
| `reasoning_summary` | `auto` | Reasoning summary preference |
| `reasoning_compat` | `think-tags` | Reasoning output mode; also accepts `legacy`, `current`, and `o3` |
| `fast_mode` | `false` | Request priority service when supported |
| `enable_web_search` | `false` | Enable default web search |
| `expose_reasoning_models` | `false` | Include effort variants in model discovery |
| `model_sync` | `true` | Discover account models |
| `model_refresh_interval` | `3600` | Refresh interval in seconds; accepts fractional values |
| `debug_model` | unset | Override the upstream model for debugging |

Matching `CHATGPT_LOCAL_*` environment variables are supported. Request-level reasoning settings, ChatMock's `fast_mode`, `responses_tools`, and `responses_tool_choice` extensions, and session/cache affinity are preserved.

## Development

```sh
go test ./...
go vet ./...
go test -race ./...  # Requires CGO and a C compiler
```

To regenerate fixtures from an unmodified ChatMock checkout:

```sh
python tools/chatmock_reference.py /path/to/ChatMock
```

The generator runs selected pure reference functions without starting ChatMock or accessing credentials. For a real gateway integration check:

```sh
python3 tools/verify_auth_routes.py \
  --binary /path/to/bifrost-http --plugin /path/to/chatgpt.so
```

This test uses an isolated gateway, temporary credentials, and a local fake OAuth issuer. Tests also cover request conversion, reasoning modes, tool streams, model discovery, and OAuth lifecycle concurrency.

## Credits

FrostMock is based on [RayBytes/ChatMock](https://github.com/RayBytes/ChatMock). Much of its authentication flow, request translation, model handling, and compatibility behaviour is a Go adaptation of that project's implementation. Credit for the original work belongs to RayBytes and the ChatMock contributors.

[Bifrost](https://github.com/maximhq/bifrost) provides the gateway, plugin interfaces, routing, access controls, and model catalog.

## License

[MIT](LICENSE). The upstream ChatMock copyright and license notice are retained.
