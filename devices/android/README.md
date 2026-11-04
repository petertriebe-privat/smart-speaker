# Jarvis on Android (Fire TV app, phone app, shared library)

The Android apps of Jarvis (#108, #75), one Gradle build:

| Module | What |
|---|---|
| `shared/` | Library: the HUD renderer and the 3D hologram (assets), the overlay window, the full screen with its native video and web layers, the audio bridge and the runner for the Jarvis core |
| `firetv/` | The Fire TV app: the screen of every Jarvis device, and with `--core` a Jarvis of its own |
| `phone/` | The phone app (#75): a Jarvis of its own with the HUD upright and Jarvis' button ([below](#the-phone-app)) |

The Fire TV app does two things:

1. **Screen for every Jarvis.** Any Jarvis device (an Echo, the stick itself) sends cards over MQTT
   and the app shows them as a HUD in amber: lists, facts, recipes, timers, weather, the day,
   charts, tables, camera images, videos and web pages; Jarvis' state and words while he talks in
   the room. Design, card format and use cases: [docs/design.md](docs/design.md).
2. **A Jarvis of its own** (`install.sh --core`). The app runs the same Go program as the Echos
   (`core/`, board `firetv`): every tool, the memory, the peer link with the Echos, a device in
   Home Assistant. **Hold the remote's microphone key and talk**; Jarvis answers from the TV. The
   voice is OpenAI's Realtime API (`gpt-realtime-mini`, billed by tokens, push to talk without
   turn detection, `core/internal/live/realtime.go`); HA's select "Sprachmodell" switches to
   `gpt-realtime-2.1`, GPT Live or speech to text and back (`tvturn.go`: slower, the voice
   sounded worse). Every turn goes to the backend (`delegate`; when it takes longer than 1.5 s
   Jarvis first says a short filler like "Einen Moment, ich schaue nach"), which also decides what the
   screen shows and picks one of the templates of `show_on_screen` (`SCREEN_TEMPLATES` in
   `shared/prompts/prompts.h`: the weather of the next days, appointments, recipes, comparisons,
   values with a chart, a route, a parcel …); on the TV every answer with content is shown. No Echo needed; with Echos the stick joins them (one more device, may become primary).

## Where a card goes

| Mode | |
|---|---|
| `auto` (the tool's default) | The app chooses: hints, messages, camera stay small in the corner; alarms, web pages and videos take the screen; otherwise the whole screen when nothing plays, and while a film plays only what is too much for the corner, the film paused until the card closes |
| `overlay` | A card in the top right corner above whatever app is in front. A film keeps playing, the app below keeps the remote. Tested above Netflix, YouTube and the launcher |
| `fullscreen` | The HUD with Jarvis' hologram as avatar on the left, the card beside it. Back closes it, as do the card's time and a `clear` |
| `web` | A web page in a frame of the HUD; the remote scrolls and follows links, back goes back first |

Jarvis' own state (status cards from the device in the room): in the console when it is open; in
the corner for a conversation held through this TV; in the corner for an Echo's conversation in the
room only while no film plays.

**The console**: the app's icon on the home screen. The hologram large in the middle, the state and
the conversation below it (the last turns above), cards on top while Jarvis shows something; it
stays until back.

**Hints on every TV**: every message Jarvis sends a person (reminders, results, hints;
`people.Message`) also appears as a small card on every TV that is on; none is woken, none in night
mode.

## How it works

| Part | Module, file | Task |
|---|---|---|
| Service | firetv: `ScreenService.java` | Foreground service (microphone, media playback), one MQTT connection (Paho), status and acks, the choice of mode, pause and resume of a film |
| Core | firetv: `Core.java`; shared: `CoreProcess.java` | Runs `libjarvis.so` (core/ for ARMv7) as a child process with `JARVIS_BOARD=firetv`, its settings, the network's DNS servers and the Android id; restarts it when it ends; log in `files/run/jarvis.log` and logcat `JarvisCore` |
| Audio | shared: `AudioBridge.java`; core: `internal/audio/tv.go` | TCP on 127.0.0.1:47160: `mic` streams the remote's microphone at 16 kHz (silence while the key is not held), `play` opens an AudioTrack |
| Push to talk | core: `cmd/assistant/tvboard.go` | Sound from the remote is the key: pressed when it starts, let go after 0.4 s of silence; the turn is committed behind its last audio |
| Console | firetv: `ConsoleActivity.java` | Launcher entry: the full screen in console mode |
| Overlay | shared: `Overlay.java` | WebView window `TYPE_APPLICATION_OVERLAY`, not focusable, top right |
| Full screen | shared: `FullscreenActivity.java` | The HUD page, a native video player and a second WebView for web pages laid over the slots the page reports; console mode |
| Renderer | shared: `assets/jarvis.js`, `jarvis.css`, `card.html`, `fonts/` | Draws the card (text only as `textContent`) and the HUD; reports its height |
| Hologram | shared: `assets/jarvis3d.js`, `VoiceLevel.java` | Jarvis as a 3D hologram in glowing lines (WebGL) in the Echo ring's colours: amber at rest, turquoise listening, purple over turquoise thinking (pulsing), turquoise to white speaking; it swells with the voice heard or spoken (`AudioBridge.level`, 20 times a second) |
| Samples | `tools/samples.json`, `tools/demo.py` | One card per use case; `demo.py <room id> [names]` shows them |
| Setup | firetv: `SetupActivity.java`, `Config.java` | Broker and room from ADB extras, kept in the app's preferences |
| Boot | firetv: `BootReceiver.java` | After a restart of the stick or an update of the app: opens the console unseen (`ConsoleActivity` with `background`), which starts the service; Fire OS takes the microphone back on a restart, Home Assistant grants it again (`tvkeep.go`); only the console opened by hand asks for it |
| Keeping | core: `cmd/assistant/tvkeep.go`, `internal/firetv` (`Keep`, `KeepRunning`) | Over Home Assistant's ADB, no PC needed: the stick's Jarvis repairs his stick 30 s after start and every 10 min (Alexa's voice app off unless `fire_tv_alexa`, microphone, overlay, no microphone symbol); the primary device starts the app on every stick that has it and where it does not run |
| Icons | `tools/make_icons.py`, `tools/avatar.png` | Banner and icon from a render of the hologram (a screenshot of the console) |

MQTT topics under `jarvis/screen/<room id>` (room id as `mqtt.TopicID`: `Arbeitszimmer` →
`arbeitszimmer`):

| Topic | From | Payload |
|---|---|---|
| `status` | app, retained | `{"state": "online"\|"offline", "display": "on"\|"off", "room", "name", "media": "playing"\|"idle", "version"}`; the broker sets `offline` (last will) when the stick drops off |
| `show` | Jarvis | `{"id", "mode": "auto"\|"overlay"\|"fullscreen"\|"web", "kind", "title", "subtitle", "blocks": [...], "url", "seconds", "notify", "here"}`; the first format (`title`, `text`, `items`, `image`) still works ([docs/design.md](docs/design.md)) |
| `clear` | Jarvis | empty: hides what is shown (the console stays) |
| `ack` | app | `<id> <mode>[ paused]` once the card is on the screen |

## Install

Needs: ADB debugging on the stick (Settings > My Fire TV > Developer options), a JDK 17, the
Android SDK (command-line tools, platform 34, build-tools 34, platform-tools) and Go. On the PC
they lie portable under `E:\Android`, the script's default; elsewhere set `JAVA_HOME_17` and
`ANDROID_HOME`.

```sh
devices/android/tools/install.sh <stick host> <room> [name] [--core]
```

The script builds the core for ARMv7 into the APK (`firetv/src/main/jniLibs`, not in git), builds
the debug APK, installs it, grants the overlay permission (`appops set dev.jarvis.screen
SYSTEM_ALERT_WINDOW allow`: Fire OS has no settings page for it) and gives the app the broker and
its room. With `--core` it also grants the microphone and puts the household's secrets
(`local/secrets.json`) into the app's private files (`run-as`, the debug
build). The first ADB connection from a computer asks on the TV to allow it ("always allow"). The
room must be the one Jarvis knows (the HA area of the stick, the speakers' `room`); the stick's
Jarvis is called "Jarvis TV <room>" apart from the room's Echo.

The remote's microphone key opens Fire TV's Alexa as well. On the study's stick Alexa's voice app
is disabled at the owner's wish: `adb shell pm disable-user --user 0 com.amazon.vizzini` (back:
`pm enable com.amazon.vizzini`). Films by title (`fire_tv` action `alexa`, Amazon's cloud) still
work without it.

Test without Jarvis: publish a card to `jarvis/screen/<room id>/show` with any MQTT client;
`tools/demo.py` shows the samples.

**Updates without the PC (#126):** the CI builds the app for every pull request (workflow
`jarvis-build`, job `firetv-build`), and a merge into main installs it on every stick from the
Raspberry Pi (`services/deployer/firetv_deploy.py`): re-signed with the installed app's key, as an
update that keeps the settings and secrets, checked and rolled back when the new version does not
start. Only the first setup of a stick needs `install.sh`. Switched on with the repository
variable `FIRETV_DEPLOY=on`.

## The phone app

A Jarvis of its own on an Android phone (#75, design: [mobile.md](../atoms3r/docs/mobile.md)).
At home and away: installed with `--device`, the broker, Home Assistant and the primary's calls go
over the home gateway, in the LAN or through the VPS ([services/home-gateway](../../services/home-gateway/README.md)).

| Part | Module, file | Task |
|---|---|---|
| Console | phone: `PhoneActivity.java`; shared: `FullscreenActivity.java`, `assets/` | The app's icon: asks for the microphone, starts the service, opens the Fire TV's console. Upright the HUD is a stage 1080 wide and as tall as the screen (`body.portrait` in `jarvis.css`): the hologram on top, the conversation or Jarvis' card below, larger type; turned sideways it is the TV's HUD. An app, not a web page: no pinch zoom, Android's font size does not grow the text (`setTextZoom(100)`; cards were cut), the system bars hidden (a swipe shows them), nothing to select, wide tables and charts stay inside the card |
| Playing with the orb | shared: `jarvis.js` (`orbPlay`), `jarvis3d.js` (`drag`, `excite`, `pulse`, `wild`) | A finger near the hologram: one violet plasma filament reaches from its core to each finger and wavers after it, as in a plasma ball, glowing where it touches the glass; the sphere glows violet and the phone ticks softly. Held, the fingers charge it (faster with more): it fills with plasma filaments, glows, grows and spins up until it explodes (a flash, shockwaves, sparks, a rumble) and builds itself anew; dragged across the sphere it turns, keeps spinning and slows down, tilted it springs back; a double tap sends a shockwave and one strong pulse; five quick taps spin it wild through every colour. Cards (they scroll) and Jarvis' button keep their own touches |
| Jarvis' button | shared: `jarvis.js` (`talkButton`), `AudioBridge.press/release`; core: `cmd/assistant/phoneboard.go` | A small arc reactor at the bottom (in the corner sideways): amber breathing at rest, turquoise waves with the voice while the microphone is open, purple while Jarvis thinks, pulsing with his voice while he speaks. A press opens the microphone at once; on release a short press was a tap: a GPT Live conversation that goes on hands free (it hears when the user is done; tapped again it ends), a long one was push to talk as on the Fire TV (`gpt-realtime-mini`, the turn committed on release, the microphone only while held). The core says over the `keys` connection when a conversation needs the microphone (`mic 1` / `mic 0`). A new question clears the last card. While a push-to-talk conversation waits with the microphone closed, the console says "Halten zum Sprechen", not "Ich höre" |
| Wake word | phone: `PhoneCore.java`; shared: `FullscreenActivity` (`setAwake`) | While the console is open in front the microphone stays open plainly (`VOICE_RECOGNITION`, no communication mode) and the core listens for "Jarvis" (the household's model from `tools/wakeword`, `install_phone.sh` with `JARVIS_WAKEMODEL`; else the built-in "Hey Jarvis"), cutoff 0.85 as the Echos. Its conversation is GPT Live; in the home the phone takes part in the Echos' wake word arbitration |
| Captions | core: `screenSend`, `phoneCard` | Jarvis' state and words go to the app over the button connection (`card {…}`, every 100 ms), not over the broker: through the data centre they lagged |
| Audio | shared: `AudioBridge.forPhone`; core: `internal/live/bargein.go` | The microphone only while held or in a conversation (or for the wake word, above). On the phone's own loudspeaker Jarvis plays as media and the microphone records plainly: the call path sounded like a phone call. His voice is taken off the microphone by our own echo canceller (`EchoCanceller.java`, SpeexDSP 1.2.1's `mdf.c` and preprocessor in `shared/src/main/cpp`, BSD, built into the phone app only): the voice is kept in a 4 s ring at 16 kHz at the moment Android says it leaves the loudspeaker (`AudioTrack.getTimestamp`), each microphone frame takes the reference of its recording moment (`AudioRecord.getTimestamp`) 32 ms earlier (measured: the echo came 40-44 ms after a reference taken 10 ms early), the canceller's tail is 256 ms. So he does not hear himself and the user can interrupt him by voice; the log says every 5 s while he speaks how many dB were taken off. The loudspeaker distorts at high volume, so the linear canceller takes off only a few dB and the preprocessor the rest. Tuning: `run-as dev.jarvis.phone touch files/run/aec-dump` records 30 s of microphone, reference and result (`aec-*.raw`, 16 kHz s16le; pull with `adb exec-out`), whose cross-correlation gives the lag. A level detector could not do it: his echo reached the microphone as loud as the user (peaks 0.7-1.0, 01.11.2026). A tap while he speaks interrupts him too (`barge`, `live/bargein.go`); a tap while he is quiet ends the conversation. Without the library (the Fire TV) the app sends the core silence while he is audible. With earbuds, a headset or the car the conversation goes over the call path (`VOICE_COMMUNICATION`, communication mode, his voice on that device), as their microphone is reached only so |
| Service | phone: `PhoneService.java` | Foreground service (microphone, media playback); the phone as Jarvis' screen over MQTT like a stick (`jarvis/screen/<room id>`, the room "Handy" by default): display "on" only while the console is open in front, so Jarvis shows cards on the phone only then |
| Core | phone: `PhoneCore.java` | `libjarvis.so` (core/ for arm64) with `JARVIS_BOARD=phone`: never primary (`primary` false, `no_lead`), no wake word, GPT Live for a tap, the Realtime API for a hold; on the phone every answer with content is shown; the phone's DNS servers and public ones after them |
| Battery | shared: `AudioBridge`; core: `audio/mic.go`, `phoneboard.go` (`phoneQuietEvery`) | With the microphone closed the app sends the core nothing (it sent 50 frames of silence a second, the wake word model ran on them) and the core's microphone poll slows to 5 a second; the agenda and the mirror of the primary's state are refreshed every 30 min instead of every 5 and 2 (a conversation asks the primary for its context anyway). Measured with the screen off, 60 s: the core 1.21 s of CPU a minute before, 0.29 s after; the app 0.36 s, 0.22 s (01.11.2026). The console's 3D hologram costs most while it is open in front |
| Notifications | core: `cmd/assistant/later.go`, `internal/screen` (`Notify`); phone: `Notices.java`, `BootReceiver.java` | `notify_phone` (from any Jarvis): a card as a notification on every phone with the app (its status says `"kind": "phone"`), its screen on or off, on the car's screen too (`CarAppExtender`); a tap opens the card in the console, with the console in front it shows at once. `research_later`: a long research in the background; Jarvis says he will come back and ends the conversation (the music plays on), and when it is done he starts a conversation where it was asked (the music pauses) and the phones get the result as a notification. The service starts after a restart of the phone (`BootReceiver`): notifications arrive at once, the microphone only after the app was opened (Android allows none from the boot); opening it promotes the service |
| Phone tools | core: `cmd/assistant/phonetools.go`; phone: `PhoneTools.java`, `MessageListener.java` | Jarvis operates the phone (only in the phone's own conversations): `phone_navigate` (the car's navigation while Android Auto runs Jarvis, else Google Maps), `phone_call` and `phone_sms` (by name from the phone's contacts or a number), `phone_messages` and `phone_reply` (the messengers' notifications, answered through their own reply action as Android Auto does), `phone_media`, `phone_open_app`, `phone_alarm` (alarm or timer). Over the button connection: `tool <id> {name, args}`, `result <id> "<text>"`. Calls and messages only after the user agreed (`LIVE_PHONE_ABILITIES`). `install_phone.sh` grants calls, contacts, SMS, drawing over other apps (Android's exemption for starting Maps, a call or an app from the background) and notification access. Test without a conversation: `run-as dev.jarvis.phone <app>/lib/arm64/libjarvis.so -ctl files/run/ctl.sock ctl tool phone_messages '{}'` |
| Android Auto | phone: `car/JarvisCarService.java`, `JarvisScreen.java`, `CarState.java`, `CarSurface.java`, `CardRenderer.java` | Jarvis on the car's screen (Car App Library; there is no category for assistants: IoT, and weather so that he may draw). Car API 7+: the HUD live (`MapWithContentTemplate`, permissions `ACCESS_SURFACE`, `MAP_TEMPLATES`): the HUD's own page (`card.html?mode=fullscreen&car=1`, without bars and avatar, the phone's upright layout in a near-square space) on a virtual display drawn straight into the car's surface, fitted to the free part beside Android Auto's panel (`carArea`): his hologram moving in his state's colour with the turn's words, or the last card he showed until the next question; the panel (size and place are the car's) holds his state and the buttons. Car API 4–6: a `PaneTemplate` with his state, the last question and his answer as rows, and a picture of the card or the hologram (the same page on an unseen virtual display, read through an `ImageReader`). The template is redrawn at most once a second. Buttons: "Sprechen" (a hands-free conversation), while one runs "Unterbrechen" and "Beenden". While Android Auto is connected (`CarConnection`) the app listens for "Jarvis", the phone's screen off too; the conversation goes over the car's hands-free (the call path). Sideloaded: in the Android Auto app on the phone tap its version ten times, then ⋮ → developer settings → "Unknown sources" |

Install (USB debugging on, the phone connected or paired for wireless debugging):

```sh
devices/android/tools/install_phone.sh [serial] [--room <name>] [--name <name>] [--device <name>]
```

`--device` pushes the phone's certificate for the way home (`services/home-gateway/make_certs.sh`).
From a worktree without `local/` the environment names another checkout's files:
`JARVIS_SECRETS` (its `secrets.json`), `HOME_GATEWAY_LOCAL` (its `home-gateway` folder),
`JARVIS_WAKEMODEL` (a folder with the household's wake word model; default `local/wakeword`).

The script builds the core for arm64 (`phone/src/main/jniLibs`, not in git) and the debug APK,
installs it with the Play Store as installer (Android Auto lists sideloaded apps only so), grants
the microphone, contacts, calls, SMS, the overlay and the notification listener, and puts the household's secrets into the app's private files
without the landline's (`SIP_*`: the phone does not ring for the FritzBox; the app reads its MQTT
broker from them too, no password on an `am start` line: adbd writes those into the phone's log),
gives the screen's name and starts the service (`SetupActivity`, in front: a microphone service may
start only then, Android 14). After a restart of the phone open Jarvis once.

## Findings

- Fire TV Stick 4K 2nd gen (`AFTKM`), Fire OS 8.1.8.5 = Android 11 with a **32-bit userspace**
  (`armeabi-v7a`): the core is built `GOOS=linux GOARCH=arm GOARM=7`, as for the Echo; arm64
  binaries do not run. `GOOS=android` would need cgo and the NDK.
- Android's seccomp filter ends a linux-built Go program with **SIGSYS** for calls an app may not
  make: `settimeofday` (the Echo's clock sync; Android keeps the clock) and `pidfd_send_signal`
  (Go checks pidfd before the first child process; Bluetooth music started `pactl`). On the stick
  Jarvis skips both and catches SIGSYS to log instead of crash (`tvboard.go`).
- No `/etc/resolv.conf` on Android: the app passes the network's DNS servers (`JARVIS_DNS`,
  `internal/netx/dns.go`). Android hides the MAC address: the HA device id comes from the Android
  id (`JARVIS_ID`).
- **The remote's microphone**: an app holding an `AudioRecord` (VOICE_RECOGNITION) receives it
  while the key is held; the key itself never reaches the app. The audio starting is the signal.
- Android executes files only from an app's native library directory: the core is packed as
  `lib/armeabi-v7a/libjarvis.so` with `extractNativeLibs`.
- Fire TV's screensaver lies above every app and overlay. A card wakes the stick first (a wake
  lock with `ACQUIRE_CAUSES_WAKEUP`, held a second): that ends the screensaver.
- Amazon's WebView draws video behind the page (the HUD covered it): videos play in a native
  `VideoView` laid over the page's slot. Web pages go into a second WebView for the same reason
  (many sites refuse iframes).
- The stick's WebView has 960 × 540 CSS px: the HUD is laid out on a 1920 × 1080 stage and scaled.
- WebGL runs smoothly: the hologram (~6 300 line quads, ~600 points) in the overlay above a film and
  in the full screen.
- Overlays work after the `appops` grant, also above DRM video (Netflix). Screenshots show DRM video
  black, so a test above a film needs a look at the TV.
- `adb exec-out screencap -p` hung or gave a broken file; `screencap -p /sdcard/x.png` and `adb pull`
  work. In Git Bash set `MSYS_NO_PATHCONV=1`, else `/sdcard` becomes a Windows path.
- Starting the service directly (`am start-foreground-service`) does not interrupt the app in front;
  an activity start would.
- Wikimedia refuses thumbnails in sizes it does not offer (HTTP 400).
- Fire OS's SystemUI shows a microphone symbol at the top right (`MicrophoneCaptureIndicator`)
  while any app holds an `AudioRecord`, over every app. Jarvis needs it open to notice the
  remote's key (the audio is the only signal). SystemUI leaves out the packages in
  `settings global sysui_mic_disclosure_exempt` (comma separated), read when it starts:
  `install.sh` sets it, it takes effect after the stick's next restart.
- Fire OS takes the microphone grant back: once ~40 s after a reinstall ("permissions revoked",
  the app killed), and on a restart (29.10.2026). `AudioRecord` then stays uninitialised (the log:
  "remote microphone: … uninitialized AudioRecord") and the key goes to Alexa. The console asks
  for the microphone when it is missing (opening Jarvis from the apps); `pm grant … RECORD_AUDIO`
  works as well.
- A foreground service started by the boot broadcast gets no microphone (Android 11: only one
  started while the app is in front does). The boot receiver therefore opens an activity first;
  the overlay permission allows that from the background.
- Graphics memory is the stick's limit (1.7 GB RAM, ~350 MB free with Fire OS). The console took
  237 MB ("GL mtrack" in `dumpsys meminfo`): the avatar hidden by opacity rendered as a second
  WebGL sphere. The stick swapped and the hologram froze for 1.5–3.5 s every ~10 s
  (`OpenGLRenderer: Davey!`). A hidden sphere now pauses and shrinks its buffer: 83 MB, no stalls.
  WebGL canvases are sized by their scaled size on the screen, not their layout size.
- A restart also enabled Alexa's voice app again (`com.amazon.vizzini`). Jarvis disables it
  himself over HA's ADB (`tvkeep.go`); by hand: `pm disable-user --user 0 com.amazon.vizzini`.
- Fire OS' home screen lists no app without an installer: installed by plain `adb install`,
  Jarvis was found only under Settings > Applications. `install.sh` installs with
  `-i com.amazon.venezia` (the Appstore, as every other app); `pm set-installer` afterwards is
  refused.
- The 4K TVs (Fire TV Stick 4K Max, a TV with Fire TV built in) run 3840 × 2160 but give apps
  1920 × 1080; their WebView reported twice the app's `devicePixelRatio`. Web pages and videos,
  placed in the app's pixels from the page's numbers, landed at twice their place, down in the
  corner. The page now takes the app's own width (`Card.width`, `devicePixels` in jarvis.js); the
  hologram's canvas too (166 MB of graphics memory on a 4K TV before, 94 MB after).
- Two MQTT clients with the same id throw each other out every second, and no card arrives. The
  service's stop (the setup restarts it) left its client open when "offline" could not be sent;
  it is now closed in any case.
- The corner card was too large on the TVs: it keeps its 440 px layout in a window half as wide
  (a quarter of the area), 12 dp from the corner.
- `adbd` refuses ADB from the stick itself: a connection from 127.0.0.1 or the stick's own address
  is closed right after CNXN (tested with our own client, which works from the PC). So the stick
  is repaired through Home Assistant's ADB connection, which is always there.
- `gpt-realtime-mini` said "ich schaue nach" and never called `delegate`; forced to call it
  (`tool_choice`), it once wrote the call as text (`{"task": …}`), which is taken as the call.
- A card stays above a video Jarvis started only in the corner: the full screen is used only while
  it is in front itself (`FullscreenActivity.visible`); a trailer played behind it before.

### The phone and Android Auto

- The phone's call path (`VOICE_COMMUNICATION`) cancels its own echo but sounds like a phone call;
  a level gate cut the user off at Jarvis' echo peaks (0.7–1.0 of full scale). Our own echo
  canceller (SpeexDSP 1.2.1 `mdf.c` and `preprocess.c`, BSD, through the NDK) takes his voice off
  the plain microphone, its reference timed with the `AudioTrack` and `AudioRecord` timestamps.
  The native library is linked with `-z max-page-size=16384` (Android 15's 16 KB pages).
- `adb shell cat` of a binary file breaks it (line endings); `adb exec-out` does not.
- Android Auto lists a sideloaded app only with the Play Store as its installer
  (`adb install -i com.android.vending`), and shows its notifications only then.
- Android Auto keeps an app's icon in a cache: a new icon showed after the next connection. A
  square picture as icon got a white disc around it; an adaptive icon (the hologram on the HUD's
  dark ground) has none.
- A notification's tap with the app's plain launch intent only brought the running console to
  the front; with its own action and data the card arrives (`Notices.java`).
- Templates only for an IoT app; a weather app may draw on the car screen's surface (the HUD
  live, `CarSurface.java`, mobile.md). A WebView on a private `VirtualDisplay` (a `Presentation`)
  renders into the car's `Surface` directly; in an `ImageReader` a still page sends no new frames,
  so `CardRenderer` keeps the last one. The first frame is black for ~3 s while WebGL starts.
- Desktop Head Unit: after a lost link the phone's adbd can keep the old forward open, the next
  unit gets no video ("Don't have video focus"); `adb tcpip 5555` restarts adbd, and the head unit
  server in Android Auto must be stopped and started again.
- In the car with music the phone's microphone clips (peak 32768); a transcription of the wake
  word's clip reads empty there, real or false (mobile.md, Activation).

## Open

- Rules for hints on the TV by voice ("beim Film nur Wichtiges", per TV): today every hint goes to
  every TV that is on.
- The doorbell's camera and HA's alarms as cards (an HA automation can publish to
  `jarvis/screen/<room>/show` itself).
- Spotify Connect on the stick; the stick's SIP registration rings for the landline like an Echo.
- The idle screen as Fire TV's screensaver; a photo slideshow; QR codes.
- The M5 port of `show_on_screen` (#110).
