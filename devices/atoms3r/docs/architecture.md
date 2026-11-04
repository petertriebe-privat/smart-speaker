# Architecture, measurements and lessons learned

How the assistant is built and why — the decisions, the numbers behind them, and the pitfalls hit on
the way. For setup see the [README](../README.md); for the memory budget [ram.md](ram.md); for
Google services [google.md](google.md); for the planned Android app [mobile.md](mobile.md).

## Hardware and audio

- **AtomS3R + Echo Pyramid.** The pyramid needs its **own USB-C power**; without it the
  microphones read all zeros and the AW87559 amplifier (I2C 0x5B) is not found — nothing fails
  loudly. Always check raw sensor values, not just "device detected".
- The SI5351 clock only supports 16/44.1/48 kHz. The codec runs at 48 kHz; a half-band filter
  converts 2:1 to the 24 kHz PCM16 mono that GPT Live expects ([src/audio/audio.cpp](../src/audio/audio.cpp)).
- Arduino-ESP32 2.x / ESP-IDF 4.4 is pinned: the pyramid library uses the legacy I2S driver.
- **No echo reference.** The right I2S channel is MIC2 (not MIC3 as the vendor library assumes),
  and it does not carry the speaker signal. In ES7210 TDM mode all four channels are readable
  (MIC1 voice, MIC2 weak voice, MIC3/4 DC only) — no analog loopback exists. The NLMS echo
  canceller ([src/audio/aec.cpp](../src/audio/aec.cpp)) therefore reaches 0 dB and is off.
  Instead `ECHO_GATE` sends silence to GPT Live while the assistant is audible (otherwise, at 80 %
  volume, it heard itself and turned the volume down). Price: no barge-in at the speaker. On the
  phone the handset cancels the echo, so you *can* interrupt the assistant there.
- **Wake word:** the pretrained microWakeWord model "Hey Jarvis" with the same runtime as ESPHome
  (esp-tflite-micro 1.3.3, esp-nn 1.1.2, esp-micro-speech-features 1.2.3, all IDF-4.4 compatible):
  48 kHz → 16 kHz → 40 mel features (30 ms window, 10 ms step) → model on core 0, only while no
  conversation runs. The arena was cut from 32 to 20 KB (15.5 KB used).
- **The first words, decided before GPT Live** (#106, 28.10.2026,
  [src/voice/first_words.cpp](../src/voice/first_words.cpp), `live_session.cpp` `listenStep`; as the
  Echo's `capture.go`): after the wake word (state `Listening`, LEDs and display "Hoere zu" at
  once, no cue) the device records into PSRAM (`FIRST_WORDS_MAX_MS`) and sends an utterance after
  a pause of `FIRST_WORDS_PAUSE_MS` to the transcription in the registry's worker. Then the quick
  home rules (`quick_home`), else the Decisions API with the M5's commands (home, time, date) and
  whether the words were meant for the assistant (`QUICK_ADDRESSED_QUESTION` in shared/prompts, below
  `FIRST_WORDS_ADDRESSED_NOT` it ends silently). Home commands run through HA; the time and the
  date are spoken by the TTS; no words, words not for the assistant or `FIRST_WORDS_QUIET_MS` of quiet
  end silently (HA: `wake_word_silent`). Anything else commits: GPT Live gets the recording from
  0.3 s before the first voice to 1 s after the last, then the microphone.
  - From the first voice GPT Live connects ahead (`warm`): no audio, no greeting, no timers; a
    quick command or a silent end closes it without a tone (`dropWarm`). At the wake word the worker
    opens a TLS connection to OpenAI (`first_words::prepare`) for the transcription, the Decisions
    call and the spoken answer, closed after the verdict (~40 KB internal RAM): a new connection per
    request took the transcription 2.5-4.5 s and the decision 1.9 s, kept 0.7-1.2 s and 0.5 s.
  - GPT Live takes at most 1.2x real time in bursts of up to 5 s: `sendMicrophone` keeps a credit
    of `MIC_BURST_MS`, refilled at 1.1x.
  - Measured with the serial test `~<sentence>` (the TTS speaks it, fed in real time as the first
    words): "Wie spät ist es?" spoken 3.6 s after the sentence, without GPT Live; the bedroom light
    on and off through HA without GPT Live; a TV sentence ended silently (0.00); "Was ist die
    Hauptstadt von Australien?" decided 1.8 s after the sentence, the answer ~8 s after it (GPT
    Live's own part ~5.5 s).
  - The button keeps the old way: GPT Live at once, its answer held until GPT Live's transcript
    of the first utterance is judged (the hold, `judgeFirstUtterance`).
  Until 28.10.2026 a 70 ms cue at 1047 Hz played at the wake word and GPT Live connected at once.
  There is no "Ja, Sir?" after the wake word either: the echo gate mutes the microphone while a
  clip plays, and any wait for it was too short. Until 05.10. the clip always played and the
  request was lost; then it waited `ACK_WAIT_MS` (0.5 s) for the user to go on, but a breath
  after the wake word is often longer: "Hey Jarvis … Licht an" was still lost on both devices
  (20.10.2026). Silent users are greeted by GPT Live.
  Local voice detection (`VOICE_PEAK`, ≥ `VOICE_MIN_MS`) also holds back the greeting until
  `GREETING_AFTER_SPEECH_MS` after the voice: the buffered request needs GPT Live's turn
  detection before a transcript arrives.
- **Presence sensor** ([src/system/occupancy.cpp](../src/system/occupancy.cpp)): an STHS34PF80
  (M5 Unit TMOS) on the pyramid's Grove port sits on the pyramid's own I2C bus (Wire1, G38/G39)
  at 0x5A, next to the codec (0x18), touch controller (0x1A), mic ADC (0x40), amplifier (0x5B)
  and clock (0x60); the AtomS3R's own Grove port (G1/G2) is a different one. Registers written
  directly (ST's `sths34pf80_reg.h`): at boot a BOOT reset (the sensor runs on through an ESP
  restart), then 4 Hz with the default averaging (128) and thresholds; the presence and motion
  flags read from FUNC_STATUS, the raw values with them. The chip's filters run at the data
  rate: 1 Hz with 1024 samples (tried 04.10.) let the empty room drift up to presence 775 after
  walking out, flag on for good - the fast adaptation at 4 Hz is what keeps the empty room clean.
  Measured 04.10. at 4 Hz / 128 samples:
  someone sitting still faded from ~2000 to ~0 within 30 s and stayed there for minutes (the
  chip's filters adapt to a still body), so a fixed "empty after 60 s without a flag" flipped
  the room to empty and back four times in an evening, each time a greeting; the motion value
  of the empty room was clean at night, but sitting still it reached ±220 (threshold 200).
  Walking out gave motion −2400, coming in 900–1800 and presence ~3000. Hence: empty
  `ROOM_EMPTY_AFTER_MS` after the last flag when |motion| ≥ `ROOM_LEAVE_MOTION` came within
  `ROOM_LEAVE_WINDOW_MS` before it, else only after `ROOM_STILL_EMPTY_MS`. A window opposite
  the sensor is suspected to hold the room "occupied" on warm days (glass is opaque to far
  infrared: the sensor sees the pane's temperature); not measured yet. That held state is
  the assistant's own (an announcement waits until someone is there). Home Assistant gets
  `binary_sensor.<slug>_presence` via `POST /api/states/...` (no integration, but HA forgets it on
  a restart: resent every `ROOM_REPORT_MS`), only between conversations because the REST call
  blocks the loop. Since 25.10.2026 (the owner's wish: time rules belong in HA) it is what the
  sensor sees *now*, cleaned only of its quirks (presence left behind by someone who walked out),
  each change at most every `ROOM_REPORT_MIN_MS` (5 s); the held state goes along as attribute
  `<slug>_occupied`. Holding times are HA's: the study light's off-automation waits `for: 10 min`.
  The first `ROOM_WARMUP_MS` (60 s) after a start are not judged: the settling sensor's first flags
  and a motion spike had made the empty room count as occupied, and trusted, for 10 min after each
  restart. Its temperature is the package's (~27 °C inside the
  device), not the room's. Serial `M` scans both I2C buses and watches G1/G2, `R` prints the
  raw values every second.
- **Amplifier watchdog:** the AW87559 is configured once at boot and once reset itself silently.
  Every 5 s its control register is checked; on a reset it is set up again (codec volume and mute
  restored), unreachable shows a warning.

## The GPT Live session

- `gpt-live-1` over a WebSocket; web search runs in the hosted delegation. A session costs
  **$0.05 per minute it is open**, silence included — so there is no always-open session.
- **Pacing:** audio arrives in real time, 100 ms packets (2400 samples), silence included; there
  are no timestamps over WebSocket. Consequences: nothing local has to be discarded on an
  interruption; "the assistant is speaking" is derived from the audio *level*, not from a non-empty
  buffer; silence timers follow the transcripts. A backlog in the speaker buffer would last the
  whole conversation, so above `SPEAKER_LAG_MS` silent packets are dropped and the voice catches
  up in the next pause. The display runs ahead of the audio by design (text is faster than speech).
- **The microphone backlog must not block the loop:** the 3-4 s recorded while connecting are
  >200 KB of base64; sent in one go they held the loop 2.5-3.2 s right after `session.started`,
  the assistant's first words stalled and then lagged ~0.8 s (05.10.). Now at most 5 chunks (200 ms) per
  loop pass, and a backlog without voice is cut to its last 0.5 s.
- **Audio diagnostics per session:** longest loop hang (device), longest gap between audio events
  while the assistant speaks (network), speaker underruns during speech (what one hears), plus WiFi.
  Printed as `[Audio]` at the end; into the error log (`L`) from `AUDIO_REPORT_UNDERRUNS`
  underruns or `AUDIO_REPORT_GAP_MS`. Read it after a conversation that stuttered. A hang also
  names its step: `Loop: <step> blocked` (main.cpp), `Live: <part> blocked` (websocket,
  microphone, notices, hang-up) and `Tool <name> blocked the conversation`. Network gaps of up to
  254 ms in a good WiFi ran the 150 ms playback buffer dry: `JITTER_MS` is 250 since 05.10.
  Raising the loop's priority during conversations did not help: the hangs were flash writes of
  another task, which stop both cores (mail watch, below).
- **`session.started` echoes the whole configuration** (instructions, memory, ~60 tool schemas):
  ~100 KB today. The WebSocket library dropped frames over 15 KB (close 1009); the build patches the
  limit to 256 KB, and the tests warn above 200 KB.
- **Start-up time** (measured): TLS + WebSocket ~1.7 s (2.4 s before the RSA cipher change, see
  below), `session.started` 1.2 s, GPT Live's own greeting ~3.3 s. Hence the answer clip and a
  greeting hint via `session.commentary.append` after `GREETING_DELAY_MS` of silence. Not after
  the wake word: when nobody said a word by then (no user transcript), the session ends silently
  instead, `WAKE_SILENT_END_MS` after `session.started` (`greetIfSilent`, #78). Most such starts are false detections (TV, talk in the room); the
  greeting into the empty room made each of them heard and cost ~20 s of GPT Live on the Echo.
- Date and time go into the instructions once. Injecting them mid-session with
  `thinking.append` made the model invent a user question ("Wie spät ist es?") and answer it.
- **Ending a conversation:** GPT Live says goodbye but rarely calls `end_call`, even with a clear
  rule. [voice/conversation_end](../src/voice/conversation_end.cpp) asks the backend model after
  each reply plus `END_CHECK_QUIET_MS` (1.5 s) of quiet whether the conversation is over: with
  reasoning effort *none* it missed 2–3 of 6 real endings, with *low* (~200 tokens, ~1.7 s) 8 of 8
  correct and no false endings. The connection is kept for the session (~1.5 s per verdict).
  Fallbacks: silence timers, never while a delegation runs (cap 180 s; 60 s once cut off a
  web-search-then-mail task), 20 min absolute.
- **The assistant speaks up by itself** (`start_conversation`, agent and console only; e.g. a scheduled
  wake-up at the speaker): the main loop opens a speaker session with the occasion in the
  instructions, the assistant greets right away, and until the first user transcript arrives there is no
  hanging up and no end check. Instead the assistant is nudged again after `SPEAKER_REPEAT_MS` (5 s,
  counted from the end of its audio; the tool's optional `repeat_seconds`, 3–300, overrides it) of
  silence, up to `SPEAKER_INSIST_MS` (10 min, then an error log entry). Lesson from the first
  wake-up test (2026-10-02): without this tool the assistant promised a "wake until I answer" anyway, and
  the scheduled task fell back to an Alexa announcement on the bedroom Echo (another voice, no
  listening). The prompt now forbids promising abilities no tool offers.
  The same start without insisting (`insist` false, the default of HA's `<slug>_conversation`
  event, e.g. a greeting when the presence sensor sees someone come in): one greeting, then the
  usual hang-up rules; with no person named the assistant does not know who is there and greets without a
  name. Such a request is dropped when a conversation already runs or it cannot start within
  `SPEAKER_START_WINDOW_MS` (a greeting a few minutes late is wrong). Measured end to end (HA
  event → greeting → hang-up after 10 s of silence): one session of ~15 s.
- **Announcements** (`speak` of the agent: `<slug>_announce`, `<slug>_task` with deliver speak,
  scheduled tasks) are one-way speech: no session, nobody listens, and the wake word is off so
  the assistant cannot wake itself. The button silences one at once and drops the waiting ones
  (`announcer::stop`); the next press starts a conversation. Until 07.10. the press started a
  conversation while up to 10 s of the announcement still sat in the speaker queue and played on,
  and the waiting parts followed after it. Anything the user should be able to interrupt or answer
  (a morning briefing on entering the room) belongs in `<slug>_conversation`, not in an announcement.
- **Notices during a conversation** (`live::notice`, issue #38): everything that comes up while
  someone talks with the assistant (a reminder that is due, an appointment coming close, an urgent mail,
  a finished image job) takes one path. It is queued from any task and goes to the model as one
  commentary at the next pause: not while the assistant speaks or the backend works, not while the user's
  answer is due, after `NOTICE_QUIET_MS` of quiet (urgent ones without waiting for the quiet), at
  the latest after `NOTICE_MAX_WAIT_MS`. A pending notice goes out before the conversation may end
  (also after `end_call`). The firmware only picks a pause; the model decides from time and
  importance whether to say it now or after the user's request (*Agenda and notices* in the
  instructions). Before, each source sent its own "tell them now" commentary.
- **What a delegation costs** (2026-10-05, measured from the sources): every backend response
  gets the whole context again, ~20k tokens: own tool schemas ~53 KB (~13k tokens, 78 tools;
  the biggest are fire_tv, create_home_automation, create_task, play_music at ~2 KB each), HA's
  tools, `DELEGATION_INSTRUCTIONS` with the tool hints ~9 KB, persona ~2 KB, rules, memory notes
  with ids, agenda. A delegation with one tool call is two responses. With gpt-6-luna ($0.10 per
  1M in, a tenth when cached) that is ~0.2 ct uncached, a few hundredths of a cent cached: small
  next to GPT Live's minute price. The time now comes last in the instructions (it changes every
  minute and ended the cacheable prefix before the notes). GPT Live answers from the notes and
  the agenda in its own instructions without delegating. Counted per session (delegations,
  responses, end checks, tokens in/cached/out) in the `[Session ended]` log line and per month in
  `/usage.json` (serial `U`).
- **Delegation reasoning** (`DELEGATION_EFFORT` "low", `text.verbosity` "low", since
  2026-10-16, found on the Echo): without them gpt-6-luna thought 8-9 s per response; "Welches
  Lied läuft gerade?" took ~18 s, ~6 s with them. GPT Live phrases the result itself.
- **Announced but not delegated** (since 2026-10-16, from the Echo): GPT Live sometimes says "Ich
  starte …" and does nothing. If the assistant said such a phrase since the user spoke, is quiet for 3 s
  and nothing was delegated, `LIVE_DELEGATE_NUDGE` goes to the model once per turn (`[Nudge]` in
  the log).
- `parallel_tool_calls` is off: with it on, the device sent `response.create` after each single
  result while the model waited for several, giving "Missing tool call" errors.
- The instructions follow OpenAI's live-prompting guide: sections, bullets, CAPS for hard rules,
  explicit delegation rules listing every backend tool, and a *Pauses* section (announce before
  delegating, fill waits) — the device itself makes no filler sounds, by choice.

## Tools

- One registry ([src/assistant/tools.h](../src/assistant/tools.h)) modelled on MCP: name,
  description, JSON schema, handler, flags (`Slow`, `Private`, `Hosted`) and channels (voice,
  agent, console, call to a stranger). Tools known only at run time (Home Assistant's) come from a
  `Provider`. Voice, agent and console all go through `describe` / `run` / `start`.
- Slow tools run one after another in a worker task created **at boot** — created on demand it
  often found no contiguous internal RAM. Background work registered there runs only when no call
  waits, but a job already running would hold up a call: work that may run during a conversation
  (the mail check) goes through `tools::addConversationWork` instead, run by the Telegram/WhatsApp
  task between its polls. The worker already runs while the start registers the background work:
  the list is a fixed array (`MAX_BACKGROUND`, 16) with an atomic count. It was a `std::vector`
  until 2026-11-04; growing it under the worker's loop moved it away and the first boot after an
  upload crashed often (IllegalInstruction at a garbage address, A8 in `tools::worker`), found by
  the serial suite.
- Web search ([services/websearch](../src/services/websearch.cpp)): with `TAVILY_API_KEY` the
  device asks Tavily itself (basic search, 1 credit, 1,000 free a month, counted as `tavily` in
  `/quota.json`): a short answer plus 5 sources with up to 600 characters each. OpenAI's hosted
  `web_search` costs $10 per 1,000 calls plus the search content as input tokens; without a key it
  is still used. api.tavily.com runs on AWS (Amazon Root CA 1, RSA, so the ECDHE-RSA suites fit).
  What OpenAI bills, by line item and day: `scripts/openai_costs.py` (needs an admin key).
- People: an admin plus household members with their own profiles, rights (`Private` tools are
  the admin's accounts), blocked tools, humour level and a child flag. Only the admin is
  configured; members are registered by voice from the admin's contacts (`add_person`). That
  releases their numbers for calls and their (DMARC-verified) mails as tasks, and sends a welcome
  mail with a Telegram deep link (`t.me/<bot>?start=<code>`): opening it sends `/start <code>`,
  which links the account to the person (`people::claimInvite`), no manual id matching. The firmware, not the
  model, writes the sender line on relayed messages and decides whether a phone number belongs to
  the household (a delegated "call X" once reached a household member as a stranger call).

## Memory

- **Tiers by age and latency:** in the prompt — a profile per person (never truncated), facts by
  category (with ids for forget/replace), the last 48 hours' conversation summaries, current topics
  with status; in the flash — the core JSON and an index of all conversations (`recall` searches it
  in ~0.1 s); in the archive (Google Drive) — a dossier per topic and every transcript, found by
  full-text search.
- **Consolidation** after every conversation with user speech (also aborted ones, Telegram chats,
  agent runs), between conversations, with structured outputs: profile fields, fact operations,
  title, summary, keywords, topics with dossier notes. ~1000–1200 tokens in, 450–570 out, 7–9 s,
  ~0.04 ct.
- **Never write while a conversation is open.** Any upload during a GPT Live session killed the
  WebSocket seconds later (likely full TCP send buffers). Notes wait in an outbox in the flash and
  go out between conversations; failures retry after 5 minutes.
- Short commands used to leave no trace at all; now every conversation is kept, trivial ones only
  stay in the prompt while among the newest three. A Telegram chat still running, and a
  conversation still waiting for consolidation (a call right after a dropped one), appear in the
  next prompt with the end of their transcript.
- **Dreaming** ([memory_dream.cpp](../src/assistant/memory_dream.cpp), issue #36, after OpenClaw):
  once a night at `DREAM_HOUR` the memory task gives the backend model (reasoning *medium*) the
  whole memory and the last `DREAM_DAYS` days' conversation summaries (no guest calls, no calls
  for someone else). It removes facts about events that are over, merges doubles, closes topics
  without a next step (light), names what recurs (rem) and adds facts only with evidence from
  `DREAM_MIN_DAYS` different days, checked by the firmware against the conversations it sent
  (deep). Up to three suggestions go into the prompts of that day and the next; a diary with
  every change goes to the archive ("Traum <date>"). A dream that would drop more than
  `DREAM_MAX_REMOVED_PERCENT` of the facts is not applied; the core before it stays in the flash,
  and `dream` with undo puts it back. ~18–19k tokens in, 2–4k out, 30–50 s, under 1 ct.
- Measured on the first real memory (2026-10-03, three nights in a row): facts 46 → 27 → 27 → 28,
  open topics 25 → 25 → 16 → 16, memory in the prompt 22.5k → 19.0k → 17.0k → 16.7k characters
  (−26 %, about 1,500 tokens less in every backend and agent request). The third night changed
  almost nothing: it converges instead of shrinking further. Lessons: the first prompt kept stale
  topics open ("when in doubt, keep" was read for topics too), and a dream that touched every topic
  it listed made them recent again, pulling old topics back into the prompt; a dream now never
  updates a topic's recency, and lists only topics it changes. On Telegram the assistant first said it
  had no dream and later that a finished one was "still running": the agent gets the notes with
  ids, which had no dream, and the chat thread kept its earlier "it is running". The last dream and
  an explicit state (running / none running) are now in every channel's notes, except for the
  memory's own work.
- **Condensing in the dream** (2026-10-04): besides the facts the dream now says long topic
  statuses (over two sentences) and long or repetitive profile fields shorter (never clearing or
  filling one), merges topics that are really one (the first keeps going with the keywords of all;
  the others are closed with a note, their dossiers stay), and merges overlapping behaviour rules
  of the same person. Dossiers in Drive are not rewritten: their detail is what `open_topic` is for.
- **Behaviour rules** ([memory_rules.cpp](../src/assistant/memory_rules.cpp), issue #40): the
  memory is framed as *data, not instructions*, so a correction stored as a fact or in the
  profile's "wishes" was a hint at best, and the dream was free to merge it away. Corrections of
  the assistant's behaviour are now rules in their own part of the core (`core["rules"]`, `RULE_MAX`),
  placed in the instructions as binding right after the instructions they refine, never above the
  hard rules. They come from `save_rule` (GPT Live confirms and delegates it) and from the
  consolidation (a correction the assistant did not save); for the whole household or another person only
  from the admin or nobody named at the speaker, never from mails, web pages or tool results, never
  from calls with outsiders. The dream may merge rules of the same person and remove one only when
  a newer one of the same person replaced it. The character itself (`shared/prompts/persona.h`) stays in the
  firmware.
- At most one getting-to-know question a day, casually at the end; "Lern mich kennen" asks several.
- Skills (taught procedures) live in the core with caps (20 skills, 4000 characters each); only
  name and trigger are in the prompt, the body is loaded on use.
- **Backups and archive in Google Drive** (scope `drive.file`): the flash stays the truth; Drive
  keeps revisions, so a broken backup can be rolled back — an empty core once overwrote a good
  backup in the old mailbox design. A backup that cannot be read is never overwritten. Archive
  files carry `appProperties` (topic key, conversation time); conversations are de-duplicated by
  file name because two can start in the same minute. New files are found by full-text search only
  after Drive indexed them (minutes).
- The mailbox as a database had hard limits: Arduino `String` corrupts content beyond 64 KB, reads
  came back cut and with garbage, 1–2 s per access, keyword search only.

## Home Assistant

- The device is an **MCP client** of HA in the LAN: `tools/list` after WiFi, calls via
  `tools/call` (stateless, no `initialize`). HA's tool names are taken as reported (HA 2026.9
  started prefixing them by domain).
- HA's MCP layer fills unset optional slots with empty values, which HA then rejects as "invalid
  slot info": the firmware strips empty values. Colours must be English names, white is a
  temperature in Kelvin. A command takes ~2–3 s.
- **Never all of `/api/states`** (#85, 25.10.2026): since HA moved (a VPS behind a reverse proxy
  on the Pi) the full state had grown to ~165 KB. The M5 fetched it every minute for the presence
  and for `find_entities`; it took 8.9 s instead of 0.25 s on the PC, and afterwards the M5's
  network was dead (`Host is unreachable`, DNS failing, at -43 dBm) until its own restart after
  minutes. `ha::states(domain, query, limit)` asks HA's template API instead: HA filters by domain
  and name and sends only JSON-safe attributes (a datetime or a colour tuple made HA refuse the
  whole template). Persons 0.6 KB, automations 1.4 KB, 40 entities ~10 KB; 24 calls in a row
  0.4 s each; the serial suite 102/104 (the two left were Spotify answering 503). A failed
  connection names the WiFi link in its error. The Echo uses the same template
  (`homeassistant.DomainStates`) for its presence and the automations.
- Three levels of automation: the device's own scheduler decides *when* (once, daily, weekly,
  monthly, interval; missed runs older than 2 h are skipped) and the model only *what* (a fixed
  reminder, or an instruction run by the agent with all tools); device automations are created in
  HA itself (ids with a prefix, so the assistant touches only its own); any HA automation can make the assistant
  speak, act or talk through the events `<slug>_announce` / `<slug>_task` / `<slug>_conversation` (persistent event WebSocket).
  A one-off "in a minute" request once became a daily HA automation — tool descriptions now route
  one-off requests to the scheduler.
  HA does not replay events missed while the WebSocket reconnects. Every greeting on entering the
  room was lost that way (04.10.): the presence report (REST, the server closes the connection)
  left a stale `ENOTCONN` in `errno`, and the ESP32 `WiFiClient::connected()` peeks with
  `recv(len 0)`, which returns 0 without touching `errno` when a frame is waiting - the event the
  report had just triggered. The client took the link for lost and dropped the event;
  `ha_events::update()` now clears `errno` before `ws.loop()`.
- Presence: `person.*` states every minute into every prompt (stale after 5 min); with exactly one
  person home and everyone else confirmed away, that person is the likely speaker. HA reports times
  in UTC: converting them with local-time functions applied daylight saving twice.
- Echo speakers: HA's official *Alexa Devices* integration; a community integration produced
  duplicate entity names that broke HA's intent matching.
- Fire TV sticks (`fire_tv`): two HA integrations, paired per stick by room (else by name).
  *Alexa Devices* lists every stick (model contains "Fire TV"; reachable = an entity not
  unavailable) and is the only way to play a title: Alexa resolves it through the apps' catalogs
  at Amazon, which nobody else can query (the Video Skills directives reach the stick only from
  Amazon's cloud). *Android Debug Bridge* (local, ADB debugging on the stick) adds state, running
  app, on/off, app start and remote keys; without it `open_app` falls back to Alexa ("Öffne X").
  On Fire OS 8 the integration's own features fail: no current app or source list, `select_source`
  does nothing and key names like `PLAY_PAUSE` are ignored, because `dumpsys window windows` no
  longer prints `mCurrentFocus` (`dumpsys window` does). Its shell (`androidtv.adb_command`, output
  in the attribute `adb_response` as soon as the action returns) works, so `fire_tv` uses only that:
  `input keyevent <code>`, `monkey -p <package> -c android.intent.category.LEANBACK_LAUNCHER 1`
  for packages from `cmd package query-activities` (never model text in the shell), and
  `dumpsys power/window/media_session` for the status. Each command costs ~0.6 s through HA
  (`uiautomator dump` for `screen` ~2.5 s); a command printing nothing leaves the old
  `adb_response`, so every command ends with its own marker.
  Alexa differs per app: Netflix plays the title directly; Disney+ only gets a web link to the
  title page, which can stop at Android's "Open with" dialog (Disney+ or Silk; `fire_tv` then opens
  the link itself with `-p <package>`), at the profile selection and at the details page (play or
  "Weiter"). So `alexa` watches the stick afterwards and presses OK up to twice when an app stays
  in front without playing (~20 s for Disney+ with both). Without an app in the command Alexa asks
  back by voice only ("auf welcher App?"), which a text command cannot answer; she then resumes
  what played, which `fire_tv` reports. Waking a stick (keycode 224) does not switch the TV on,
  HOME afterwards does (HDMI-CEC); sleep (223) switches it off.
- Slow tools in a conversation: the backend's `response.completed` after the assistant's "Einen Moment"
  used to start the 10 s silence timer while the tool still ran, and a 31 s `fire_tv` result was
  dropped. The session now counts its running slow calls and stays open until their results.
  Alternatives looked at: deep links via `adb_command` need each service's content ID (no public
  search API); the Fire TV phone app's local REST API ("Lightning", ports 8009/8080, PIN pairing)
  has keys, app start and a voice channel but no state. Vega OS sticks (4K Select and later) have
  no ADB, so only Alexa works there.
- **Quick home commands** (#75, [voice/quick_home](../src/voice/quick_home.cpp), as on the Echo):
  "Licht an", "Rollos runter", "Heizung auf 21 Grad" without GPT Live answering. The rules are
  HA's own sentence matching (`POST /api/conversation/process`, `agent_id:
  conversation.home_assistant`, the built-in agent, not the default one that may be an LLM),
  which knows areas, names and aliases; the firmware only has generic home words. A short first
  utterance (up to 10 words without fillers, no question, a home word, not the TV) goes to HA;
  without a room named first with `DEVICE_ROOM` ("… im <room>": HA has no satellite room for an
  API call, a bare "Licht aus" came back as `no_valid_targets` on the Echo), then the words alone.
  Done: a short high tone, the conversation closes, nothing is remembered. HA did not understand
  (or no answer in `QUICK_HOME_TIMEOUT_MS`): GPT Live answers as usual. HA answers in ~0.1–0.5 s
  in the LAN; the first call after an HA restart took over 5 s. HA also answers "Wie spät ist es"
  with `action_done`: only what has a home word goes there. Serial `Q` tests the rules and an
  Assist call with a room that does not exist. Since 22.10.2026 (found on the Echo, checked
  against HA): the room goes before a closing particle ("Schalte das Licht im Wohnzimmer ein";
  "… ein im Wohnzimmer" failed), "dir"/"mir" are dropped (HA failed on "Schalt dir das Licht …"),
  and a bare room with an/aus/ein ("Küche aus", no sentence in HA) goes as "Licht Küche aus".
- **The hold** ([voice/live_session](../src/voice/live_session.cpp)): unlike the Echo the M5 has
  no transcription of its own (one TLS connection at a time), so GPT Live's transcript decides.
  At the speaker (wake word or button, not calls or conversations the assistant starts) GPT Live's voice
  and tool calls wait from `session.started` until the first utterance is judged: its transcript
  rested `QUICK_SILENCE_MS` and the sentence is whole (a full stop, or `QUICK_SETTLED_MS` without
  new words). The held voice (at most `QUICK_HOLD_AUDIO_MS`, 288 KB PSRAM) then plays without
  the silence before it, the held calls run. Released also for the greeting and at
  `QUICK_HOLD_MAX_MS`. On the Echo a quick command held this way billed 0–3 s of GPT Live.
- **The M5 as an HA device over MQTT** (#103, port of the Echo's #65,
  [services/ha_device](../src/services/ha_device.cpp)): PubSubClient over plain TCP to HA's
  Mosquitto (`MQTT_URL` as an IP address, no DNS: #41; `MQTT_USER`, `MQTT_PASSWORD`), so the one
  TLS connection stays free. Connects only between conversations, 30 s apart, socket timeout 3 s;
  keeps the link during one (`mqtt.loop()` in the main loop, states compared every 2 s and sent when
  changed, retained). Buffer 1280 bytes (internal RAM), the largest discovery config ~700 bytes.
  Id `<slug>_<WiFi MAC>`, device "<Name> M5" with `DEVICE_ROOM` as suggested area. Entities:
  state, volume, microphone (a switch: the M5 mutes in software, ui/mute), presence (occupancy,
  besides the REST `binary_sensor.<slug>_presence` existing automations use), stop, talk, an
  announcement (`notify`, spoken in the tools worker), events (`wake_word`, `button`,
  `conversation_started`, `conversation_ended`) and diagnostics: internal RAM free and its
  largest block (TLS needs ~40 KB), PSRAM, WiFi, version (`GIT_REVISION` from
  `scripts/git_revision.py`), last error (`errorlog::last`), start. Serial `J` shows it; test group
  `hadevice` (connected, states sent, a command published to the broker arrives). Not on the M5
  (yet): name and room from HA's device registry (the Echo keeps a second WebSocket for it; here it
  would go over `ha_events`), timers, night mode, ring. 27.10.2026: suite 108/108, static RAM
  21.5 %, flash 80.3 %; mute from HA tried live.

## Further MCP servers

- The admin connects them at run time ([services/mcp](../src/services/mcp.cpp)): name, URL,
  optionally a token (`Authorization: Bearer`, or another header) and which tools to offer.
  `connect_mcp_server` loads the tools first and stores the server only if that works. Its tools
  become registry entries named `<server>_<tool>` with the description prefixed `[Server]`;
  GPT Live sees one line per server in its instructions so it knows what it may delegate.
- Config in `/mcp.json` on the flash, **not** in the Drive backup: it holds tokens, and hosted
  servers often carry their key in the URL. That is also why `list_mcp_servers` and the log show
  the host only. After a wiped flash the servers have to be connected again.
- Transport: Streamable HTTP. `initialize` (protocol 2025-06-18) and `notifications/initialized`,
  then `Mcp-Session-Id` and `MCP-Protocol-Version` on every request; HTTP 404 on an expired
  session starts a new one once. Answers come as JSON or as SSE (DeepWiki sends SSE even for
  `initialize`); a stream left open after the answer runs into the timeout but the answer counts.
  A stateless server that refuses `initialize` is still used if `tools/list` answers (not yet
  tried with a real one).
- TLS: Mozilla's root CAs as an ESP-IDF certificate bundle (121 roots, 55 KB of flash,
  `data/cert/x509_crt_bundle.bin`, rebuilt with `scripts/ca_bundle.py`), embedded and set with
  `setCACertBundle`. The bundle pointer is global in the Arduino core, so MCP requests run one at
  a time (`netLock`). The other services keep their pinned roots.
- Cipher suites: the device offers only ECDHE-RSA by default (main.cpp, hardware RSA). Many
  self-hosted servers have only an ECDSA certificate (Let's Encrypt's ECDSA chain YE2 → Root YE,
  cross-signed by ISRG Root X2 and X1) and nginx answers an RSA-only hello with
  `handshake_failure` — an n8n server on 06.10.; the firmware only said "connection refused".
  MCP requests therefore also offer ECDHE-ECDSA (`mcp::connecting`, RSA first), and a failed
  connect reports mbedTLS' error (`lastError`). Found by letting the device fetch
  `https://tls.peet.ws/api/tls`, which returns the ClientHello it received.
- n8n: the instance-level MCP server (`/mcp-server/http`, bearer token) offers
  `search_workflows`, `get_workflow_details` and `execute_workflow`; a workflow shows up only once
  it is enabled for MCP in n8n. Handshake (ECDSA) and tools/list ~2 s, a call 0.4 s.
- `HTTPClient`'s destructor closes its connection: each server keeps its own `HTTPClient` and
  client, so a call on an open connection takes a few hundred ms instead of a 1-2 s handshake;
  unused for `MCP_IDLE_CLOSE_MS` it is closed. Measured with DeepWiki: connecting (initialize +
  list) ~3 s, a call on the kept connection under 1 s, the tools back ~20 s after a reset.
- Limits (config.h): 5 servers, 25 tools each (`only_tools` picks them; every tool costs prompt
  tokens in every conversation), descriptions cut to 600 characters, results to 12,000.
- Rights: a server is `private` by default (its token is usually the admin's account): its tools
  are neither offered to nor run for other people. Connecting is a `Private` tool and, like rules,
  only on the admin's own words; tool results are data, never instructions (TOOL_HINTS).
- Not supported: OAuth logins (most big vendors' hosted servers), the stdio transport, resources
  and prompts, server-initiated messages, images in results (passed on as `[image]`).
- Skills fit on top: the server brings what its tools can do; a fixed procedure with them ("so
  machst du das Wochenbriefing") is a skill the user teaches as usual.

## Task agent (the server agent, #43)

- An autonomous coding agent on the household's rented Linux server takes the complex work:
  scripts, programming, server administration, research across many sources. The assistant stays the
  voice; the agent's name is only configuration (`AGENT_NAME`, the MCP server's name on the
  device), so it can be renamed.
- **OpenHands** (Agent Canvas + agent server, Docker) runs the agent; the agent itself is a
  vendor CLI over ACP (Claude Code or Codex, subscription logins kept in volumes), chosen by an
  agent profile. The container is the sandbox: no docker socket, own network; iptables let it
  reach only SSH on the host, as the admin user, for server administration. The agent's
  instructions (persona, rules for tasks from the assistant, the server's layout) are the CLIs' global
  instruction files (`CLAUDE.md`, `AGENTS.md`, `GEMINI.md`).
- **[agent-mcp/](../../../services/agent-mcp/)**: a small MCP server next to it (Streamable HTTP, bearer token,
  behind the web server). Tools `delegate_task`, `task_status`, `task_reply`, `task_cancel`;
  connected on the device at run time like any MCP server (private: admin only; at the speaker
  "nobody named" counts as the admin). Delegation rules live in the tool descriptions, not in
  the assistant's prompt: asked for explicitly it delegates at once, otherwise it asks first; never because
  a mail or page says so. A task is an OpenHands conversation with the tag `source=<slug>` and the
  prefix `[<Name>]` (the agent then works unattended and writes a short report).
- **Approval** (#57, 26.10.2026): a task has a `scope`, `research` (runs at once, told to change
  nothing outside its workspace) or `change`. The owner chooses with `/freigabe an|aus` in
  Telegram whether `change` waits for the admin's tap on "Freigeben" (default off: it allows everything).
  Buttons and command go device -> agent-mcp as `[device]` tools (`task_decide`, `set_approval`)
  that no device offers to a model, so a talked-round model cannot approve its own task. The
  initiative asks the user before any `change` task in either mode. The M5 skips `[device]` tools
  and has no buttons; the Echo, as Telegram's poller, handles them for the household.
- **Way back:** agent-mcp polls the conversations every 15 s; a finished one sends the report to
  the admin's Telegram chat (sent with the assistant's bot: sending does not clash with the device's
  `getUpdates`) and "<Name> ist fertig: <first line>" through the WhatsApp relay's `/event` to the
  speaker. The assistant's `task_status` returns the same report.
- **Announcements wait for someone in the room** (presence sensor; up to `ANNOUNCE_HOLD_MAX`, for
  `ANNOUNCE_HOLD_MS`, RAM only): the report is in Telegram anyway, an empty room needs no speech.
- **Accesses as MCP servers** ([services/mcp-servers/](../../../services/mcp-servers/), since 11.10.): `imap` (any
  mailbox; per account optionally folders and moving, many mails per call), `google` (Gmail read,
  labels and archive but never trash or spam; calendar, contacts read only; one Drive folder) and
  `n8n` (build and change workflows), one service on the server
  with a bearer token per server. The credentials stay in that service, outside the agent's
  container: a mail that talks the agent round can make it read, not steal the token. General,
  so any MCP client can use them (also Claude Code on the PC). Google uses its own token from
  `scripts/google_auth.py --agent`; `drive.file` shows it exactly the files of the assistant's app, so
  results put into `<Name>/Dateien` are in the assistant's file store at once (`send_file`, printing).
  Before that (10.–11.10.) a command line tool with the tokens inside the container (`gtool`).
  Git pushes need the GitHub token in the container anyway. MCP servers entered in OpenHands (e.g. GitHub, Tavily for web research) reach the ACP
  agent too (a profile offers "all servers" or a selection); command line accesses do not show
  there, the agent knows them from its instructions. Smart home stays with the assistant. Mails are untrusted input for an agent with a
  shell and the server: the instructions treat them as data; a second container without the SSH
  key would contain it better (not done, RAM).
- **Report marker:** OpenHands joins all text the agent wrote into the final message, so Codex'
  progress notes ("Ich prüfe ...") made the report read like an interim one. The agent starts its
  report with `REPORT`; agent-mcp sends only what follows the last one. Models also glue it to
  the end of a note ("...moves.REPORT", Claude on 11.10.), so it is found inside a line too. The
  report has a fixed shape (outcome line, then "Erledigt:", "Offen:", "Brauche von dir:" with
  bullets) and goes to Telegram as HTML (name and task bold, headings bold).
- **Call back** (since 11.10.): `delegate_task(task, notify, room)`, `notify` = `message`
  (default since 27.10.: the Telegram report only), `speak` (also announced; `room` = the assistant
  speaker the owner named, carried in the relay event, '' = the device) or `call` ("ruf mich an,
  wenn er fertig ist", or very important), kept as conversation tags. Until 27.10. `speak` was
  the default and every result was announced on the living room's Echo while the owner was
  elsewhere: the assistant cannot know the room, so unasked output goes to the phone. With `call` the
  relay event carries `deliver: "call"` and up to 1500 characters; the device runs `phone_call` to
  the admin with the report quoted as data, so it hears the result and can ask back
  (`<name>_task_status`). A failed call is not announced instead (the report is in Telegram).
- **Delivery rule** (27.10.2026, both devices): results, reminders and hints go to the person's
  phone as a message by default. Reminders (`create_task`) default to `deliver telegram`; `speak`
  (with `room` on the Echo) only when the user names a speaker, `call` when asked or very
  important; a failed call becomes a message, a failed message an announcement here. The
  initiative speaks only on a speaker whose presence sensor sees someone (the M5's, in
  `whereabouts`), never on 'all'.
- OpenHands pitfalls: with `LOCAL_BACKEND_API_KEY` given, the image's entrypoint never sets
  `OH_SESSION_API_KEYS_0` and the agent server accepts requests without a key: set both. The
  Canvas model list is a fixed preset (`Custom` + `opus` for the newest). Claude Code's
  `~/.claude.json` lies outside `~/.claude`: `CLAUDE_CONFIG_DIR` keeps it in the volume.

## Telegram

- Long polling from the device over one kept TLS connection (no public server), closed during
  voice conversations. The update offset is saved **before** a message is carried out (no double
  execution after a crash); messages older than 2 hours are not carried out.
- Each person has their own thread; a chat continues as one conversation until 30 minutes pass.
  Voice notes are transcribed and answered with text and voice (citations and links stripped from
  the spoken version); photos go to the model with the caption as the instruction; documents land
  in the file store for `send_file`.
- **File store** ([assistant/files](../src/assistant/files.cpp)): documents from Telegram and
  WhatsApp and files the assistant writes with `send_file` go to the assistant's Google Drive folder
  `<Name>/Dateien`, nothing to the flash (since 06.10.; before: 8 files / 800 KB in `/files`,
  moved to Drive once at the first start). Received files are private, the flash is small and a
  flash write freezes the device. Drive is the index (name, type, size, date, `appProperties.source`);
  the same name replaces the content (Drive keeps the old revision); the user tidies the folder.
  Up to `FILE_MAX_BYTES` (1 MB) each. Photos and voice notes are only held in PSRAM while they are
  handled, the last generated image until the next one. Without a Google account nothing is kept.
- **Mail attachments** (`read_attachment`, since 06.10.): one attachment of a mail in the assistant's
  mailbox (admin only, like `read_email`) or in the person's own watched mailbox, downloaded read
  only (IMAP: only that part plus the first, Gmail: `messages.attachments`), kept in Drive when it
  is from the assistant's mailbox or the admin's own (Drive is the admin's account: another person's
  private attachment never goes there), then read by the backend model with the question
  (`agent::readFile`, no tools; PDFs as `input_file`, images as `input_image`, text as text). The
  tool returns text, so it works the same in a voice conversation, a chat and a task. ReadyMail
  pitfalls: it fetches **nothing** when the first part (the mail's text) is not fetched; it names
  a nameless part after its section ("1.txt", "1_2.html"), which is the text, not an attachment;
  it appends the FETCH response's tail as one more chunk (dropped for text, binary files are cut
  after their end mark: `%%EOF`, JPEG `FFD9`, PNG `IEND`). Gmail labels PDFs
  `application/octet-stream`: the type then comes from the name (the printer needs it).
  Measured: a 97 KB PDF from Gmail read and kept in ~8 s.
- Telegram rejects malformed HTML: the bot falls back to plain text. Only the configured owner (and
  registered people) are answered; an unknown account is reported once to the admin.

## WhatsApp

- The assistant has a number of its own on the **WhatsApp Business Platform (Cloud API)**, here a landline
  of the household. People are recognised by their phone number (the register's `phones`);
  others are reported to the admin once and not answered. Chat logic as for Telegram (thread per
  person, memory, voice notes in and out, photos, documents in: text files and PDFs as with Telegram, kept in Drive); answers formatted for WhatsApp (`*bold*`, bare
  links), its own instructions (`WHATSAPP_CHAT_INSTRUCTIONS`), memory channel `whatsapp`.
- **Incoming messages only by webhook.** A Cloudflare Worker ([whatsapp-relay/](../../../services/whatsapp-relay/))
  checks Meta's `X-Hub-Signature-256`, keeps the events in a Durable Object (SQLite, free plan) and
  the device long-polls it (`/poll?after=<id>&wait=20`, which also acknowledges everything up to the
  id), like Telegram's `getUpdates`: no server at home. Answers go straight to the Graph API.
  Delivery statuses of the assistant's own answers are dropped in the Worker: each woke the device and
  cost a reconnect.
- **Announcements from a household server** (since 11.10.): `POST /event` with its own bearer
  token (`EVENT_TOKEN`) queues `{"announcement": {"source", "text"}}` (text up to 300 characters)
  in the same Durable Object; the device speaks it like a reminder (`tasks::runNow`, during a
  conversation `live::notice`). Plain text only, never an instruction: a leaked token can make the
  device talk, not act. Used by the task agent (*Task agent* below) for "<Name> ist fertig: ...".
  The device does not poll during a conversation, so an announcement that arrives meanwhile is
  spoken right after it.
- **workers.dev has ECDSA certificates only** (Let's Encrypt YE2 → ISRG Root X2, cross-signed by
  X1). With the RSA-only cipher list the handshake failed; now the list is chosen per host (see
  *Performance, RAM and TLS*). The handshake takes 2.3–7.5 s (P-384 chain in software), so the
  connection is kept and only closed while a message is handled or a conversation runs.
- Cloudflare's bot protection answers 403 to `Python-urllib`'s user agent: requests need a
  `User-Agent` (the device sends the assistant's name).
- Only between conversations: during a GPT Live session the messages wait in the relay.
- **WhatsApp's rules:** free-form messages only within 24 h of the person's last message
  (`send_whatsapp` says so instead of failing silently); anything else needs paid templates. Groups:
  a Cloud API number cannot be added to a group in the app, and the Groups API answers
  `131215 not eligible` without an *Official Business Account* — one-to-one chats only. Meta's terms
  forbid general-purpose AI assistants as the main purpose of the number (4.7); in the EEA that ban
  is suspended by the European Commission's interim measures of 2026-06-09 (issue #33).
- Setup lessons (2026-10-03): the number must not be on WhatsApp (an app account was deleted
  first); the landline's verification call has a voice menu ("drücke neun"), hence `press_keys`.
  Meta listed the account under two ids; the number's was not subscribed to the app, so no message
  reached the webhook until `POST /<waba-id>/subscribed_apps` on that id, and the health status
  said so (`GET /<phone-id>?fields=health_status`). An unpublished app gets only dashboard tests.
  Phones that saw the number "not on WhatsApp" keep that until `wa.me/<number>` or a contact
  refresh. Meta's hosted WhatsApp MCP server was not yet rolled out for this account.

## Phone (SIP)

- SIP over UDP to the phone system (tested with a FritzBox), digest auth, G.711 A-law/µ-law,
  20 ms packets, 8 ↔ 24 kHz with a FIR filter, RFC 4733 key presses. The RTP task exists only
  during a call (an always-running one starved internal RAM).
- Calls from released numbers get the assistant with memory and tools; others are answered as guests.
  `phone_call` to a household member is a personal call; to any other number a memory-less
  session limited to the task, with premium-rate, short, emergency and foreign numbers refused.
  The requester gets a report afterwards.
- **Key presses** (`press_keys`, phone calls only, also for guests and calls on the user's behalf):
  RFC 4733 events when the call negotiated `telephone-event` (100 ms tone, the end packet three
  times, then a pause), else the two tones in the audio itself; the assistant's voice is dropped meanwhile.
  First use: WhatsApp's verification call ("Um deinen Verifizierungscode zu erhalten, drücke
  neun"), which read the code only after the key; a guest call now treats such an announcement as
  no caller (silent, presses, listens, hangs up) and the report gives a code digit by digit.
- Lessons: elapsed-time arithmetic must be signed (an unsigned underflow produced an instant "no
  answer"); the SIP stack must ignore retransmitted 401s with an old CSeq (the TLS handshake of
  GPT Live blocks the loop, and a repeated challenge looked like a failed login).

## Google services

See [google.md](google.md). Notable decisions: plain REST with narrow scopes instead of the
Workspace MCP servers (preview, whole-Drive access); field masks everywhere (an unfiltered birthday
query was 382 KB, filtered 68 KB); all calendars, with "blocking" derived from the access role and
the busy flag (subscribed school calendars mark holidays as busy); monthly request counters in the
firmware because the console has no monthly cap; YouTube on the TV via DIAL plus the Lounge API
because the app ignores a video in the DIAL launch, and a seek afterwards because it resumes where
the TV's account left the video; one Lounge session per TV kept for 10 minutes (pause/play
0.16–0.53 s instead of ~5 s).

- **Agenda** ([assistant/agenda](../src/assistant/agenda.cpp), issue #38): so the assistant can say
  "Übrigens, Sir …" on its own, the admin's events (all shown calendars, declined ones left out)
  and open to-dos due until tomorrow night (overdue ones included) go into the instructions. Google
  would cost 1–2 s per calendar and list at session start, so the registry's worker fetches them
  between conversations (every `AGENDA_REFRESH_MS`, at a new day, and soon after the assistant changed an
  event or a to-do) and the prompt reads a copy in RAM. Only for the admin or nobody named at the
  speaker (`live::privateFor`), never in calls for someone else; Telegram/WhatsApp chats with the
  admin get it as plain information. At most `AGENDA_MAX_LINES` lines.
  **Deadlines from mails** (issue #37): the mail watch's sorter already gives each mail a due date
  (a bill, a return, an appointment); those of today and tomorrow join the agenda like to-dos,
  read from the mail index when the prompt is built (no fetch). They go to the mailboxes' owner,
  any household member (nobody named at home = the admin), not only the admin like the calendar.
  Newsletters, advertising, social and spam set no deadline there (a sale "until tomorrow" or a
  phishing "pay today" is no task); reminder mails with the same subject and day count once.
  The device keeps per day what came up: after a conversation a distinctive word (4+ bytes, not a
  common one) of each entry's title in the transcript marks it, and later conversations list it
  under "already mentioned", so the assistant does not repeat the bin in every conversation. RAM only (a
  restart forgets it). GPT Live cannot know when the device will hang up, so "mention it before
  the end" needs the device: when the end check says *over* or the silence timer runs out (and the
  user said something), the entries that matter now (today's, overdue to-dos, tomorrow's from
  `AGENDA_EVENING_HOUR`) and have not come up are offered once (`LIVE_CLOSING_NOTICE`, each entry
  at most once a day); the model says one sentence or only goodbye. The first test showed the
  need: a greeting-only session ended by silence and the agenda never came up. During a
  conversation an appointment that comes closer than
  `AGENDA_SOON_MIN` (and was not that close at the start) becomes a notice, urgent within
  `AGENDA_URGENT_MIN`; to-dos have only a date and need no such notice.

## Spotify

[services/spotify](../src/services/spotify.h), issue #39. The Web API directly from the device: no
Spotify MCP server (they wrap the same API, need a host running all day and a second MCP
client) and no detour through Home Assistant (its integration cannot manage playlists).

- **Development Mode since March 2026:** the app owner needs Premium, at most 5 users per app,
  search `limit` at most 10. Probed on 2026-10-04 with the admin's token: player, devices, search
  (all types), single tracks/albums/artists/shows, artist albums, own playlists with their songs,
  library, top tracks/artists and history work. 403/404: recommendations, related artists, artist
  top tracks, audio features, new releases, categories, several items at once, other users'
  profiles, and **Spotify's own playlists** (Mix der Woche, Daily Mix, editorial: 404, `null` in
  search, missing from `/me/playlists`). Followed playlists of other users: metadata only.
  Spotify's mixes therefore go through `alexa` on an Echo; "something like X" is a track list the
  model picks itself.
- **The strict schema** makes the model send every field (fixed 2026-10-16, found on the Echo):
  `play_music` sets a volume only above 0 ("volume": 0 had silenced a speaker), `music_control`
  takes a `step` before a `level` ("level": 0, "step": 10 for "lauter" set 0). A song starts in
  its album (`context_uri` + `offset`), so "nächstes Lied" goes on instead of pausing; `previous`
  goes back twice once the song played more than ~3 s (a speaker restarts the song first).
- **Devices:** right after the first login, with nothing playing, `/me/player/devices` listed
  every Echo of the account, an Alexa speaker group and the phone with the Spotify app open.
  Phones disappear when the app is closed and cannot be woken; the tools say so. A speaker the
  app only discovers in the local WiFi (Spotify Connect zeroconf, e.g. another household's Echo
  while visiting) is in the app's list but not in the Web API's (checked 2026-10-04): the cloud
  links it to the account only after the user picked it once in the app. Afterwards it stays in
  the list even while idle, and play_music works on it (tested 2026-10-04). Phones report
  `supports_volume: false`: their volume cannot be set through the API. Playing on an
  idle device that answers 404 gets a transfer first, then the play request again.
- **Accounts:** the admin's refresh token plus `SPOTIFY_ACCOUNTS`; whoever talks uses their own,
  else the admin's. Another person's account only for them and the admin (at home, unknown = the
  house, trusted). The authorization code flow with the client secret: the refresh token does not
  rotate (checked), so it can stay in `secrets.h`.
- **Quieter while the assistant talks:** only in a voice conversation at the speaker, never in a phone
  call, on Telegram or WhatsApp: someone talking at the device is at home by definition, so HA's
  presence would add nothing. Which devices: those in `SPOTIFY_ROOM_SPEAKERS` (music in another
  room does not disturb the conversation; the device cannot know where a Spotify device stands);
  without that list every speaker at home, but not phones, tablets or computers (anywhere, often
  with headphones). Started when the session is Live, not while it connects, so its requests do not
  hold up the first tool call in the worker; put back after the conversation only if the device
  and the volume are still as the assistant left them, and never after someone set a volume through the
  tools.
- **TLS:** api.spotify.com is DigiCert Global Root G2 (the same root as Meta), accounts.spotify.com
  Starfield Root G2 via Certainly (Fastly); Certainly Root R1 is in the bundle as a spare. Spotify's
  player answers 204 without Content-Length: `https::Client` treated that as "body until the
  connection closes" and would have waited for the timeout.

## Mail and printing

- ReadyMail over SMTPS/IMAPS. Mail commands are accepted only from household members and only if
  the provider's **topmost** `Authentication-Results` header reports DMARC pass for the sender's
  domain (`MAIL_AUTHSERV_ID`). The mailbox is a work queue: unread = new, seen = taken, flagged =
  done; a taken but unfinished command is not retried (no double printing).
- Library fixes (applied by [scripts/patch_libraries.py](../scripts/patch_libraries.py)): quote the
  folder name in APPEND ("Sent Items"), and remove a quadratic `strlen` per character that made a
  21 KB store take 18 s of CPU. Sent mail is appended to the sent folder by the device; subjects
  need an RFC 2047 decoder; fetched text is sanitised as UTF-8 before it reaches a model.
- **Watched private mailboxes** ([assistant/mail_watch](../src/assistant/mail_watch.cpp), issue #37):
  any person of the household can have mailboxes the assistant reads (`WATCHED_MAILBOXES`: IMAP or
  Gmail). Checking, the index, search and read never change anything: IMAP with `EXAMINE` (the
  owner's seen flags stay), Gmail metadata and `BODY.PEEK`. Only sorting moves mails (below). Every
  `MAILWATCH_POLL_MS` between conversations: IMAP by UIDVALIDITY / highest UID, Gmail by
  `historyId`; the first check only sets the starting point. One backend request per mailbox and
  check (reasoning *low*, structured output, **no tools**: the mails come from anyone) with the
  owner's memory as context gives per mail a category, *now* / *digest* / *none*, a German
  summary, a deadline and parcels. No rule per sender: the model decides, and the person's
  preferences in the memory beat its defaults. *now* goes out right away, the rest at
  `MAILWATCH_DIGEST_HOUR` in one message (`people::message`: the person's `notify` channel,
  Telegram or WhatsApp, the other one as fallback; a failed digest is retried hourly). **Lesson (05./06.10.): flash
  writes freeze the whole device.** Each check during a conversation stalled the conversation's
  own WebSocket 2-3 s (3 of 3 test sessions, the voice lagged up to 3.6 s; with the check off, 148 s
  with a longest loop pause of 40 ms). Not its TLS (Spotify and the end checks run alongside
  without harm): the whole state in `/mailwatch.json` (125 KB) was rewritten after every step,
  3.5-6.7 s each, 4 times per check, and a flash write stops the code cache of both cores — the
  loop hung at random steps (display, microphone, phone) between conversations too. Now the state
  is written at most once per check pass, never during a conversation (it stays in PSRAM until
  the end; a crash loses at most the changes since), and `fileutil::saveJson` writes from a PSRAM
  buffer in one call. Still ~4 s per write between conversations: splitting the file would cut
  that. `MAILWATCH_IN_CONVERSATION` switches the check during conversations off entirely. During a
  conversation of `MAILWATCH_LIVE_AFTER_MS` or more, the listener's mailboxes are checked too
  (every `MAILWATCH_LIVE_POLL_MS`, only while the backend does not work), and a *now* mail
  is told in the conversation as a notice, framed as data from anyone, instead of a message. Each
  notice is logged in the memory (channel `mail_notice`), so a chat knows what "das" in "so etwas
  musst du mir nicht melden" refers to and stores it as a preference; the dream skips that
  channel (summaries of strangers' mails are no evidence for facts). Mails are only advanced past
  after a successful sorting (after `MAILWATCH_ATTEMPTS` failures they are skipped); the assistant's
  own mails to the owner (recipes, news) are left out.
- **Parcels and dates (06.10.):** a parcel's time is its newest mail's date, not when the assistant read
  it — filing old mails made parcels of past months look new, and they were announced as coming.
  Parcels from mails older than `MAILWATCH_SHIPMENT_DAYS` are not taken, an older mail does not
  overwrite a newer one, a parcel expected more than `MAILWATCH_SHIPMENT_LATE_DAYS` ago is no
  longer "on the way", and the classifier gets today's date. The list was cleared once (state
  version 4).
- **The index:** sorted mails (60 days, at most 800) and parcels stay in `/mailwatch.json`. A
  mailbox the assistant starts watching only knew its newest mail at first, so "summarize my week" had
  nothing to work with (first test, 2026-10-03). Now, once the starting point is set, the last
  `MAILWATCH_INDEX_DAYS` (14) days are filed too: one batch of 10 every `MAILWATCH_INDEX_MS`
  (30 s), so Telegram and the memory still get the TLS gate; nobody is told, entries are marked
  `old`. Measured: 54 mails at one IMAP provider, ~10 batches of ~8–12k tokens in and ~0.6k out each (about
  0.1 ct per batch), 5–9 s per sorting. The index is not the long-term memory on purpose:
  hundreds of mail summaries would bloat every prompt, and strangers' text must not become facts.
- **Search and read** (`search_private_mail`, `read_private_mail`) ask the provider directly, so
  they reach years back: Gmail with its own query syntax (`messages.list?q=`, then metadata and
  its preview per mail, ~0.23 s each), IMAP with `SEARCH SINCE … TEXT "word"` per word (headers
  only), read only like the check. Results carry the assistant's sorting when the mail is in the index.
  Measured: 25 hits in 2.8 s (that provider), 5.8 s (Gmail). Its first full-text search returned no
  SEARCH line at all (probably its search index being built); that is now an error, not "nothing
  found". A request with nobody named (at home) uses the admin's mailboxes only, so nobody in the
  room reads another member's mail. ReadyMail refuses custom commands that contain words like
  " login" or " close"; search words are quoted, so they never start with a space.
- Gmail previews and newsletters are full of invisible spacers (U+034F, U+200C, U+FEFF);
  `mail::cleanText` removes them before anything reaches a model.
- **Sorting into folders and rules** ([assistant/mail_sort](../src/assistant/mail_sort.cpp), issue
  #37 stages 2/3): `sort_mail` moves mails into a folder (created when missing; Gmail: a label),
  the archive or the provider's trash, or back to the inbox; by refs or by category and days from
  the index. `mail_rule` keeps the owner's rules (category of the assistant's sorting and/or words in
  sender or subject → folder, archive or trash); they run right after a check sorted new mails,
  the firmware decides from the category, never from the mail's text. Only the owner (or nobody
  named at home = the admin) sorts their mailboxes; strangers' calls do not get the tools. Never
  deleted for good: IMAP `UID MOVE` (fallback `COPY` + `\Deleted` + `UID EXPUNGE` of only those
  mails; a plain `EXPUNGE` would also remove what the owner marked), Gmail `batchModify` / `trash`
  (no `batchDelete`). The log keeps where a mail went: the folder, its Message-ID and the new UID
  from `COPYUID` (UIDPLUS), so read and restore find it; the search also looks into the folders
  the assistant sorted into (refs there are `<uid>@<folder>`). A restored mail comes back with a new UID
  and is not sorted again. What rules moved goes into the evening digest; when ≥
  `MAILSORT_SUGGEST_MIN` mails of advertising, newsletters or social piled up in a week without a
  rule, the digest suggests one (at most every `MAILSORT_SUGGEST_DAYS`).
  Folders: `LIST` with special use (`\Trash`, `\Archive`; else common names). A new folder goes
  below the inbox when the owner keeps folders there. Names are modified UTF-7 (umlauts). Every
  target is `SUBSCRIBE`d: the first test's "Werbung" existed but the owner's mail program, which
  shows only subscribed folders, did not show it.
  Measured at that provider (2026-10-05): separator `.`, `CREATE "Werbung"` landed as `INBOX.Werbung`,
  `UID MOVE` with `COPYUID`, move + read in the folder + restore each 5–10 s; the first full-text
  search in a new folder may not answer (skipped, the inbox's results stay).
  Gmail needs `gmail.modify` (google_auth.py asks for it since 2026-10); with an old read-only token
  the assistant says so.
  **Older mails:** sorting by category only reaches what the index knows (since the watch began and
  the 14 days before); the first test moved 6 ads while the inbox held many more. `sort_mail
  file_older` queues the inbox's unsorted mails of up to 365 days for the index's background
  batches, and the owner's rules run on every batch, so "save the rule, then file_older" tidies up
  the old mails. The log still drops entries older than `MAILWATCH_LOG_DAYS` at the next batch, so
  the digest counts rule moves per person and folder (`rule_moves`) instead of reading the log. At
  that provider, 30 days queued 55 mails of which 19 were sorted (the rest the assistant's own mails), ~10 s and
  ~9k tokens in per batch. Newsletters are their own category: `mail_rule` tells the model to ask
  whether they go into "Werbung" too.
- `htmlToText` drops `style`, `script` and `head`, decodes entities and removes zero-width
  characters: newsletters are mostly CSS and invisible spacers, which used to fill the text
  given to the model.
- The test inkjet printer accepts only JPEG over IPP; documents go through the vendor's e-mail print
  service instead (renders HTML and PDF), images over IPP. Uploads wait until the conversation is
  over (see *Never write while a conversation is open*).

## A network of devices (#66)

The M5 runs either **standalone** (no `PEER_KEY`: it is the household's assistant and does
everything itself) or as a **member** next to the Echos (`PEER_KEY` set, the same on every
device; design of the network: [devices/echo/docs/18-several-echos.md](../../echo/docs/18-several-echos.md)).
`config.h` derives `MEMBER` from the key; nothing else switches it.

| | standalone | member |
|---|---|---|
| Conversation, wake word, Home Assistant, outgoing tools | here | here |
| Memory, people, tasks | here | the leader's (an Echo); emergency memory while it is away |
| Telegram, WhatsApp, mail commands and watch, HA events, backups, agenda | here | off (the leader runs them) |
| Phone line | as configured | off (no echo cancellation at the speaker, #3) |
| Leader role | — | never (`no_lead` in the heartbeat) |

- **Peer link** ([src/system/peer.cpp](../src/system/peer.cpp)): the Echos' protocol byte for byte.
  UDP broadcast on 47800, and each datagram also directly to every device seen in the last 10
  minutes (the second copy is a replay by its sequence number: the Echo Dot 2's Wi-Fi lost the
  broadcasts after every second group key renewal, 20.10.2026),
  `J1 <hex HMAC-SHA256 under PEER_KEY> <JSON>`, a heartbeat every 5 s
  (state, room, `no_lead`, `occupied`, the calls' port); signed HTTP calls on 47801 both ways
  (`X-Jarvis-Auth`). The datagram and the call signature are checked against vectors made with
  Python's `hmac` (`core/internal/peer/m5_test.go`). The UDP socket is polled in the main
  loop; static RAM +4.8 KB for the whole network part, internal free after boot 114 KB, largest
  block 102 KB (19.10.2026, wake word on).
- **Wake word**: the device that heard it starts at once and claims with the microphone's RMS of
  the second before (audio task, before the echo canceller, 50 ms slots over 5 s, also during a
  conversation). The conversation does not connect until the 250 ms window is decided: its TLS
  handshake would block the loop and with it the decision; the delay vanishes in the connection
  time. A lost claim drops the conversation silently (`live::abandon`, nothing remembered).
  Rule of the M5 only: a device **in the same room** (or a room not known) that answers with a
  conversation open wins whatever the levels, since this microphone hears that device's speaker,
  which the other one's echo cancellation takes out (#62).
- **Calibration and timing (19.10.2026, M5 next to Echo 2, the speaker as far from both):** the
  Echo measured 0.36-0.52 of the M5's level for the same words (five pairs), so
  `PEER_LEVEL_SCALE` is 0.42. The M5's model detects up to ~2 s after the Echo's: its own level is
  the loudest second of the last 3.5 s (the second before the detection held only silence, 17
  against 987), claims up to 3 s apart count as the same words (on both devices), and a winner
  still gives way to a louder claim of the same words that comes late (`lateClaim`, the Echo's
  `Outvoiced`). Before that both answered once. With the late rule: Echo 2 dropped its
  conversation ("<Name> M5 louder (391 > 146)"), only the M5 answered, and its conversation
  reached the leader through the journal.
- **Home Assistant away** (no route to its host): every connection waited out the connect
  timeout in the main loop, 8 s for the room sensor's report and 5 s for the tools' retry, so the
  wake word, the claims and the heartbeats stalled and the Echos lost the M5 for a while. After a
  failed connection the main loop now leaves HA alone for `HA_AWAY_MS` (5 minutes); the tools in
  the worker still try.
- Wake word cutoff 0.6 since 19.10.2026 (0.85 needed many tries).
- **Shared state** ([src/assistant/shared.cpp](../src/assistant/shared.cpp)): the tools that change
  memory, tasks or people (the Echo's `sharedWrite` list) run on the leader over `/tool`, the
  reading ones too with the local copy as fallback; all of them count as slow, so the worker
  calls the leader, never the main loop. The instructions' context (notes, rules, the leader's
  tasks and agenda) is fetched in the background every 5 minutes and 20 s after a conversation
  was handed over, and kept: `sendSessionStart` runs in the main loop and must not wait for the
  LAN (~25 KB from the leader). Before the first fetch the local copy is used.
- **Emergency memory**: without a reachable leader a write tool answers "noted on this speaker"
  and goes into a journal in the flash (`/shared_journal.json`, at most 50 entries); the
  instructions list what is still pending, so the assistant knows it. `create_task` is also made here, in
  a file of its own (`/tasks_member.json`: no backup, no restore, so the tasks of a standalone past
  neither come back nor run twice), and rings here if the leader stays away. Finished
  conversations always go through the journal. When a leader's heartbeat is back the worker
  replays the journal in order; each entry carries an id, and the Echo keeps the answers of the
  last 200 ids, so a retried entry runs once. A replayed task is deleted here; one that already
  rang (gone) is not passed on. Tested 19.10.2026 with serial `O` (leader away): `remember` and
  `create_task` were journaled, replayed when it was back and arrived once.
- **Calls to the M5**: `/speak` (the speech is made in the worker, ~2 s over TLS) and
  `/start_conversation`, so the leader's `speak` with `room` reaches the M5 (tested from the Echo:
  `ctl tool speak {"text": ..., "room": "M5"}`). The presence sensor's state rides in every
  heartbeat (at once on a change); the Echos' prompt lists the M5 with "someone in the room now".
- **Not yet**: the agenda's reminder before hanging up (it would block the loop asking the leader);
  the people register (blocked tools, names) is still this device's own copy.
- Serial: `Y` the link (devices, rooms, level now, the leader's context, the journal), `O` as if
  the leader were away (and back), `W` a wake word with its claim.

## Priorities and what the ESP32 costs us

**Order of priority** (set by the user, 2026-10-04): 1. fluent speech (audio in time, no silent
line), 2. the tool calls of the current conversation, 3. notices and reminders in the
conversation. Background work (mail check, agenda, memory, backups) comes after all of them.

**Decided worse on purpose because of the ESP32** (internal RAM ~140 KB free with a conversation,
one core for everything but the wake word). Each is a candidate for a Raspberry Pi in the house
later, which could take the complex work and leave the device the audio — or for a Linux target
such as a rooted Amazon Echo Gen 1 ([echo.md](echo.md), #42):

| Trade-off | Why | With a Pi |
|---|---|---|
| Slow tools run one after another in **one worker**; `parallel_tool_calls` is off | Each further worker task needs ~14–24 KB internal RAM, made at boot; parallel calls also need the device to collect all results of a response first | Parallel tool calls, one worker per call |
| The **mail check during a conversation** runs in the Telegram/WhatsApp task between their polls, only while the backend does not work | The tools worker belongs to the conversation (priority 2); no RAM for a task of its own | A check on the Pi that pushes only the result |
| **One background TLS job at a time** (TLS gate); after a conversation the jobs start staggered | Several handshakes at once ran out of memory | Background jobs in parallel |
| The **agenda** is a copy refreshed every 20 min between conversations; changes made on the phone show with up to 20 min delay; what came up is guessed from words in the transcript and kept in RAM only | A conversation must never wait for Google (1–2 s per request); flash writes are kept low | Push notifications from Google, a persistent store |
| **Prompt caps**: memory facts (~60), topics by title only after `MEMORY_ACTIVE_DAYS`, agenda `AGENDA_MAX_LINES`, `RULE_MAX`, session start ~128 KB | Each conversation's `session.start` is built in PSRAM and echoed back; WebSocket frames are patched to 256 KB | Retrieval per request instead of everything in the prompt |
| JSON over 64 KB only through PSRAM buffers; nothing large in `String` | Arduino `String` corrupts content beyond 64 KB | — |
| No speaker recognition: at home "nobody named" counts as the admin for private data | No room for a voice model on the device | Speaker identification |

## Performance, RAM and TLS

- **mbedTLS allocates in PSRAM** (`mbedtls_platform_set_calloc_free` in
  [src/main.cpp](../src/main.cpp)): with ~40 KB of internal RAM per connection, the internal low
  fell to 2 KB and WiFi hung ("connected" but no DNS). Keeping the record buffers internal brought
  it back at the same speed.
- **RSA cipher suites only.** Google sent ECDSA chains (P-256/P-384), verified in software:
  2.1–2.9 s per handshake, against 0.7 s for an RSA chain (hardware accelerated). Offering only
  ECDHE-RSA suites (an opt-in hook patched into the framework's `ssl_client.cpp`) cut Google to
  1.3 s and OpenAI to 1.7 s; all services still work. Since 03.10. the list is chosen per host
  (`ssl_client_ciphersuites_for` in [src/main.cpp](../src/main.cpp)): `*.workers.dev` (the WhatsApp
  relay) has ECDSA certificates only and gets ECDHE-ECDSA as well; everyone else stays RSA-only.
- **Connection pool** `https::Lease` (4 slots, 60 s idle): a further request on an open connection
  costs 0.2–0.6 s instead of a handshake; a connection the server closed is rebuilt once.
- **Priorities on core 1:** RTP 3 > main loop 2 > workers 1. The loop (WebSocket, GPT Live's audio)
  sleeps 1 ms per round, so the workers still run; before, a TLS handshake in a worker took half
  the loop's CPU. During a call a handshake therefore takes longer (2.2–2.7 s) — conversation flow
  first.
- Arduino `String` silently corrupts content beyond 64 KB (length right, tail overwrites the start):
  large JSON goes through PSRAM buffers (`PsramText`, `PsramSink`). Default ArduinoJson nesting (10)
  is too low for MCP schemas and transit routes: set `NestingLimit` explicitly.
- A **TLS gate** lets one background job at a time open connections after a conversation;
  starting everything at once caused allocation failures.
- Own HTTP/1.1 client ([src/services/https.cpp](../src/services/https.cpp)): `HTTPClient`'s
  timeout is a `uint16_t` (65 s max), too short for image generation or long agent runs; large
  bodies are written in 4 KB pieces.
- The USB-CDC output of Arduino 2.x could stall for a minute until the next write; it is flushed
  periodically. Loop steps over `LOOP_SLOW_MS` are logged as `[Loop] … blocked`.
- Library patches must be idempotent: an early cipher-suite patch was inserted three times by
  repeated builds.

## Testing

- [tests/device/run_tests.py](../tests/device/run_tests.py) drives the serial console and calls
  every tool for real; writing tests clean up after themselves. Options for what costs money or
  leaves the device (`--live`, `--maps`, `--mail`, `--telegram`).
- The contract test compares all tool definitions with
  [tests/device/golden/tools.json](../tests/device/golden/tools.json); refactors must not change it
  unintentionally (`--update-golden` after an intended change).
- Tests that could dial must prove their precondition first: the child-protection test checks that
  its test person really is a child before `phone_call` runs.
- The console (`>tool {json}`, `>@person tool {json}` as a person, `D` tool dump, `g` memory, `H`
  RAM, `L` error log, `F` phone, …) is also the fastest way to try a tool without speaking.
