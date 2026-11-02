# Porting M5 modules to the Echo (Go)

The Echo version of the assistant is a Go program (`core`, module `echo-assistant`). It ports the
M5 version in `devices/atoms3r/src` module by module. The M5 code stays unchanged and is the
reference: behaviour, tool names, descriptions, JSON schemas and result texts are copied
**verbatim** unless the Echo hardware forces a difference (then say so in a comment).

## Rules

- Code, comments, logs and tool texts in English (as on the M5). Comment density like the
  existing Go files: a doc comment per exported item, a line where the M5 has a lesson learned.
- No household data (names, addresses, numbers, mail addresses, IPs, calendars, device names) in
  code, tests or comments. The assistant's name is configuration too (`ASSISTANT_NAME` in
  `secrets.h`, default "Jarvis"): never hard-wire it. Household values come from `config.Secrets` (the M5's `secrets.h`
  keys, e.g. `cfg.Secrets.String("HA_TOKEN")`); constants from `prompts` (the M5's
  `config.h`/`prompts.h`/`persona.h`, e.g. `prompts.Int("SPOTIFY_TIMEOUT_MS", 15000)`,
  `prompts.Text("LIVE_MODEL")`).
- Standard library only, plus what `go.mod` already has. No cgo (static ARMv7 build).
- Build checks (from `core`): `GOOS=linux GOARCH=arm go vet ./...` and `go test ./...`.
  Unit tests for pure logic (parsing, formatting) are welcome; no network in tests.
- Never deploy, never touch the device, never commit.

## Packages to build on

| Package | Use |
| --- | --- |
| `internal/tools` | `Registry.Add(tools.Tool{Name, Description, Parameters (JSON schema string), Channels, Private, Run})`, `Registry.AddProvider(...)` for run-time tool lists. `Run(args map[string]any, ctx tools.Context) string` returns the text for the model; errors start with `"Error: "`. Helpers `tools.String/Int/Bool(args, name)`. Channels: `tools.Voice`, `tools.Agent`, `tools.Console`, `tools.Stranger`, `tools.Everywhere` (default). Every call already runs in its own goroutine: blocking network calls are fine. |
| `internal/netx` | `netx.Client(timeout)` HTTP client (IPv4 only: the kernel has no IPv6; embedded root CAs). Always use it, never `http.DefaultClient`. |
| `internal/homeassistant` | `ha.CallTool(name, args)` (MCP), `ha.Rest(method, path, payload) (status, body, err)`, `homeassistant.Failure(status, body, err)` for error texts. |
| `internal/openai` | `openai.Responses(ctx, body)`, `openai.Post(...)`, `openai.Speech(...)`, `openai.Transcribe(...)`. |
| `internal/google` | `acct.Request(ctx, method, url, contentType, body, refreshToken)`, `acct.JSON(ctx, method, url, in, out)`. |
| `internal/store` | Persistent files on the data partition: `store.SaveJSON/LoadJSON/Write/Read/Append/List`. Use it where the M5 uses LittleFS or NVS. |
| `internal/prompts` | M5 constants: `prompts.Text/Int/Bool/List`. |
| `internal/config` | `config.Secrets.String(key)`. |

## Shape of a ported service

```go
// Package spotify: ... as devices/atoms3r/src/services/spotify.cpp.
package spotify

type Service struct { /* state */ }

func New(secrets config.Secrets, ...deps) *Service
func (s *Service) Configured() bool
func (s *Service) AddTools(r *tools.Registry) // registers the same tools as the M5's addTools()
```

Arduino → Go: `String` → `string`, ArduinoJson → `encoding/json` (maps or structs),
`millis()` → `time.Now()`, FreeRTOS locks → `sync.Mutex`, PSRAM buffers → slices,
`https::Client` → `netx.Client`, `Serial.printf("[X] ...")` → `log.Printf("[X] ...")`.
