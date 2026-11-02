# Assistant Memory (RAM)

As of: Sep 29, 2026, firmware `72d849d`. Updated with every new measurement.

Measure: serial `H` (total heap, free heap, largest free block, low-water mark since boot,
never-used stack per task); at boot, `[RAM boot]` logs what each step cost.

The ESP32-S3 has two memory types:
- **Internal RAM**: scarce and fast. Wi-Fi, the network stack, task stacks, and drivers need it.
  When it runs low (under ~10 KB), Wi-Fi hangs (observed on Sep 27 and Sep 29).
- **PSRAM**: 8 MB, slower, plentiful. Everything large lives there (JSON, audio buffers, TLS since Sep 28).

## Internal RAM

Total heap 328 KB (335,836 bytes), plus 63 KB of fixed global variables (`.data`/`.bss`).

| What | Used | Note |
|---|---:|---|
| System before startup (FreeRTOS, main task 8 KB stack, USB serial, timer tasks) | ~30 KB | fixed |
| Pyramid (M5Unified, I2C, codec, amplifier, I2S driver with DMA buffers) | 20.3 KB | fixed |
| Audio task (8 KB stack, ~5 KB used) + voice session | 8.9 KB | |
| Memory, people, to-do lists (filesystem, locks) | 3.1 KB | data in PSRAM |
| Tool worker + Telegram task (14 KB stack each, ~6 KB used) | 32.9 KB | |
| WhatsApp task (14 KB stack) + kept relay connection (since Oct 3) | ~16 KB | estimated, not in the idle total below |
| Phone (management only; the RTP task is created per call, 5 KB) | 0.3 KB | |
| Wi-Fi + network stack + Home Assistant events | 40.3 KB | fixed, drivers |
| Wake word (20 KB arena, 15.5 KB used; 5 KB task, 2.5 KB used; interpreter) | 38.7 KB | |
| Idle connections (Telegram polling, Home Assistant) | 7.5 KB | |
| **Free when idle** | **153.8 KB** | largest block 143 KB |
| + one conversation (WebSocket to GPT Live with TLS) | 12.3 KB | free then 141.5 KB |
| Briefly under load: memory and to-do-list tasks (14 KB each, only between conversations), parallel TLS connections | up to ~96 KB | low-water mark under full test load: 58 KB free |

## PSRAM (8 MB)

| What | Used |
|---|---:|
| Audio: microphone (8 s since 05.10., was 5 s), speaker, and wake-word buffers, session decode buffer | 946 KB |
| Phone: RTP audio buffers | 242 KB |
| GPT Live's held voice (the hold of the quick home commands, 6 s at 24 kHz, 21.10.2026; not yet in the totals below) | 288 KB |
| Display canvas | 33 KB |
| Memory core (JSON) | 28 KB |
| Network, Home Assistant events, people, to-do lists, tools | 20 KB |
| Runtime (TLS connections, HA tool list, address book, acknowledgment clips) | ~245 KB |
| **Used when idle** | **1.37 MB** (6.7 MB free) |
| + one conversation (session config ~80 KB, TLS, receive buffer) | ~100 KB |
| briefly when sending a file (`send_file`, the whole file, Sep 30) | up to 1 MB |

## History

| Date | Free when idle | Low-water mark under load | What changed |
|---|---:|---:|---|
| Sep 28 | ~2 KB → 36–51 KB | – | mbedTLS moved to PSRAM |
| Sep 29 | ~131 KB | 2.7 KB (Wi-Fi hung) | Telegram task with 24 KB stack |
| Sep 29 | ~133 KB | 27.8 KB | all background tasks 14 KB instead of 24 KB |
| Sep 29 | 153.8 KB | 58 KB | wake-word arena 20 KB, wake-word stack 5 KB, image task 14 KB |
| Sep 30 | 137.9 KB | 72.5 KB | Maps, weather, Drive backups and archive (after voice tests) |
| Sep 30 | 125.3 KB | 68.9 KB | connection pool (up to 4 open TLS connections to Google), after the full test series |
| Oct 3 | – | – | WhatsApp task and relay connection; 126.8 KB free during a WhatsApp agent run, full free test series passed (idle value not measured) |

## Projection: multiple simultaneous conversations (Sep 29, not measured)

Cost of an additional conversation, derived from the measurements above:

| | Internal | PSRAM |
|---|---:|---:|
| GPT Live session (WebSocket with TLS) | ~12–15 KB | ~100 KB |
| Briefly during setup (TLS handshake, end-of-conversation check) | +~10 KB | +~50 KB |
| Second phone channel: RTP task + UDP buffer | ~7 KB | 242 KB (second set of audio buffers) |
| Dedicated tool worker per conversation (optional; otherwise slow tools queue behind each other) | 14 KB | – |

**Pyramid + one phone call simultaneously:** 153.8 − 12.3 (conversation 1) − 13 (conversation 2) − 7
(a second RTP task would not be needed here, since the Pyramid has its own audio paths) ≈ **128 KB free**;
with a brief Telegram agent run (~20 KB) and a TLS setup (~10 KB), about **100 KB**.
The big load spikes (memory summarization, scheduled tasks, mail polling) only occur
between conversations and don't apply here. **Comfortably enough.**

**Two phone calls simultaneously:** as above, plus the second RTP task (7 KB) and 242 KB PSRAM:
about **120 KB free**, ~90 KB under load. **Also enough.** The FritzBox must allow two calls to
the same IP phone (call waiting / second line).

**Three conversations** (Pyramid + two phone calls): ~105 KB free, ~75 KB under load. Still feasible.

So memory isn't the limiting factor. The limits lie elsewhere:
- **Blocking connection setup:** The TLS handshake for a new session blocks the main loop for
  ~1.5 s (`[Loop] live blocked 1563 ms`). If a conversation is already running, it would stall
  for that long. Each session would therefore need its own task.
- **Software:** `live_session` and `phone`/`rtp` only know about a single conversation (global
  state). Refactoring to session objects would take roughly two days.
- **CPU:** two audio streams with resampling, Base64, and JSON in addition to the wake word;
  probably feasible, needs to be measured.
- **Cost:** GPT Live bills per open session ($0.05/min); two in parallel cost twice as much.
