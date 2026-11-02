# Jarvis on the TV: design and use cases

The screen app (#108) shows Jarvis as a heads-up display in amber on near black. This file is the
design language, the card format the renderer understands (`app/src/main/assets/jarvis.js`) and
the use cases it is built for. Sample cards for every use case: `tools/samples.json`; show them on
a stick with `python tools/demo.py <room id> [names]`.

## Design language

The reference is the film HUD of an AI butler: elements on concentric rings around the viewer,
segmented arcs and tick scales like a reactor core, thin wireframe lines, numbers at the edges,
everything glowing on a dark ground ([scifiinterfaces.com: Iron Man HUD, a breakdown](https://scifiinterfaces.com/2015/07/01/iron-man-hud-a-breakdown/),
[Polaine: Iron Man's HUD and interaction design](https://www.polaine.com/2008/05/iron-mans-hud-and-interaction-design/)).
Our own take, no film assets or logos:

| Element | How |
|---|---|
| Colour | One amber `#ff9a1f` with a hot `#ffc46b` and a deep `#e8590c`; text warm white `#fbf3e8`, secondary `#c4ad94`; red `#ff4b3e` only for alarms. Background `#050608`. |
| Type | Rajdhani (OFL, bundled) for headings, labels, numbers; letter-spaced capitals for eyebrows and labels. Running text in the system sans (Roboto / Amazon Ember) for reading from the sofa. |
| Panels | Chamfered corners, a 1.5 px amber edge fading in the middle, L-brackets on two corners, a faint amber gradient from the top. |
| The hologram | Jarvis himself, in 3D (`jarvis3d.js`, WebGL), drawn in glowing lines rather than a cloud of points: broad ring bands of parallel lines around the sphere, an eye in the centre (concentric rings with ticks, always facing the viewer) with spokes running out, circuit traces with nodes on the inner shells, arc dashes and small panels on the outer one, thin outline rings, soft bokeh lights and a few sparks (~6 300 line segments as screen-space quads with a bright core and a glow, ~600 points). Each shell turns slowly at its own speed and tilt, the far side is dimmed, the lines flicker a little, over a soft glow. States, eased into each other: idle (slow turn), listening (a little faster, the shells breathe), thinking (faster, more flicker), speaking (the shells swell gently, the glow pulses); all calm, nothing races. A new status updates the running sphere instead of starting it again. Without WebGL the 2D orb (segmented SVG rings) stands in; the small brand mark in the top bar stays 2D. |
| HUD frame (full screen) | Top bar: orb + JARVIS, the card's kind between two lines with a scanning light, date and clock. Bottom bar: "Zurück · Schließen", a bar running down the card's time, "Sys · Online". Background: a 48 px grid and a warm glow. Jarvis' avatar: the hologram (600 px) on the left, the card to its right unfolds from its side; the avatar shows his state while he talks about the card (`state` on any card). Web pages and videos get a smaller avatar and more room; cards with their own hologram (status, idle screen) hide it. |
| Motion | Cards rise in (0.45 s), blocks follow one after the other; icons have a turning ring. Nothing flashes except alarms and a live camera. |

Sizes: the overlay is 440 dp wide in the top right corner above any app. The full screen is laid
out on a 1920 × 1080 stage and scaled to the TV (the stick's WebView has 960 × 540 CSS px), so it
looks the same on every set; running text 25 px, headings 46 px on that stage.

## Card format

`jarvis/screen/<room id>/show` (see the README for the topics):

```json
{"id": "...", "mode": "overlay | fullscreen | web", "seconds": 0,
 "kind": "info", "eyebrow": "extra label", "title": "...", "subtitle": "...",
 "from": {"name": "Anna", "via": "WhatsApp · gerade eben"},
 "blocks": [{"type": "...", "label": "...", "wide": false, ...}]}
```

The first format (`title`, `text`, `items`, `image`) still works and becomes blocks.

**Kinds** (eyebrow and icon): `info`, `list`, `recipe`, `timer`, `weather`, `calendar`,
`message`, `alert` (red, pulsing edge), `camera`, `music`, `home`, `energy`, `route`, `package`,
`call`, `search`, `choice`, `web`, `video`; plus two layouts: `status` (orb with captions) and
`ambient` (the idle screen, full screen only).

**Blocks:**

| type | Fields | For |
|---|---|---|
| `text` | `text` | a few sentences |
| `list` | `items`: strings or `{text, done, note}` | shopping, to-dos, parcels (two columns above 8 in full screen) |
| `steps` | `items`, `current` (0-based) | recipes, instructions: past dimmed, current bright |
| `stats` | `items`: `{label, value, unit, trend}` | key numbers in tiles |
| `kv` | `items`: `{label, value}` | details, rooms, tomorrow |
| `table` | `columns`, `rows`, `highlight` | comparisons, connections; numeric columns right-aligned by themselves |
| `chart` | `kind`: line / bar, `labels`, `series`: `[{name, values}]` (max 2), `unit` | energy, temperature, prices; the peak bar is highlighted |
| `timer` | `label`, `seconds` or `ends_at` (ISO), `total` | countdown ring that runs on its own; red when done |
| `progress` | `label`, `value` 0..1, `text` | washing machine, download, battery |
| `forecast` | `now`: `{temp, condition, text, high, low}`, `items`: `{time, condition, temp, rain}` | weather; `condition` = Home Assistant's weather conditions |
| `timeline` | `items`: `{time, title, note, state: past/now}` | the day, the agenda |
| `choices` | `items`: strings or `{text, note}` | "sag die Nummer": numbered options |
| `media` | `title`, `artist`, `cover`, `playing` | now playing, with a running equalizer |
| `image` | `url`, `caption`, `live` | pictures, a camera snapshot (`live` adds a red tag) |
| `video` | `url`, `caption`, `loop` | mp4 / webm / HLS with sound; full screen in a native player laid over the slot (Amazon's WebView draws video behind the page) |
| `web` | `url` | a web page in a frame of the HUD (made from `mode: web`) |

**Web pages:** `{"mode": "web", "url": "..."}` opens the page in a frame of the HUD with its
title and address in the head. A second, native WebView lies over the frame, so sites that
refuse iframes work too; the remote scrolls and follows links, back goes back in the page's
history first, then closes. It stays until closed unless `seconds` is given.

**YouTube** goes through the stick's YouTube app (`play_video`, it can already), not this app.

## Use cases

What Jarvis can put on the TV, by situation. *Trigger*: asked (the user wants to see it),
proactive (an event from Home Assistant or Jarvis' own initiative; rules decide when, no fixed
thresholds), or companion (shown while Jarvis talks, so the answer can stay short).

### Jarvis himself
- **The hologram with live captions** (`status`): listening / thinking / speaking, what the user
  said and what Jarvis answers; small in the corner above a film, or large and free in the full
  screen. Companion to every conversation in the room.
- **Idle screen** (`ambient`): the hologram with the clock below it, weather, the rest of the day. As a screensaver or
  on "Jarvis, zeig mir den Tag".

### Everyday answers (companion)
- Weather today, tomorrow, the week, rain radar picture (`forecast`, `image`).
- Calendar: today's timeline, tomorrow, free slots (`timeline`, `kv`).
- Lists: shopping, to-dos, packing lists; ticked items stay visible struck through (`list`).
- Facts with a picture: "Wie weit ist der Mond weg?" (`text`, `image`), Wikipedia page (`web`).
- Search results and comparisons: products, prices, ratings (`table`, `text`).
- Choices the user picks by voice: restaurants, films, recipes, contacts (`choices`).
- Unit conversions, calculations, translations: a short `stats` or `kv` card.

### Kitchen and household
- Recipe step by step, the current step lit, ingredients beside it; "weiter" moves on (`steps`).
- Several timers as rings that run on the TV by themselves (`timer`).
- Washing machine, dishwasher, dryer: time left (`progress`), done as a toast.
- Parcels: what comes today, where it waits (`list` with notes).
- Bins: which one tomorrow (part of the idle screen or a toast in the evening).

### Home (Home Assistant)
- Overview: temperature, humidity, lights on, windows open, per room (`stats`, `kv`).
- Energy: consumption per hour, solar against consumption, costs (`stats`, `chart`).
- Climate history: temperature or CO₂ of a room over the day (`chart`).
- Door camera when someone rings, above the film (`camera` overlay with `live` image; a clip as
  `video`).
- Alarms: smoke, water, a window open in the rain, a storm warning (`alert`, full screen, red).
- Presence: who came home (toast).

### Messages and calls
- WhatsApp, Telegram, mail as a toast in the corner with the sender (`message` with `from`).
- Incoming call with the caller's name (`call`).
- A summary of the mail of the day (`list`).

### On the way
- Travel time by car, train, bike and the next connections (`route`: `stats`, `table`).
- Departure reminder with the countdown ("Abfahrt in 12 min", `timer`).
- A map picture of the route (`image` from a static map URL).

### Media
- Now playing on Spotify with cover and the speaker (`media`).
- Videos: news clips, a how-to, a camera clip, in the app with sound (`video`).
- Web pages: the article Jarvis talks about, a recipe site, a timetable (`mode: web`).
- Photos: a picture from the phone or Google Photos, a slideshow later.

### Knowledge and family
- Homework help: a formula, a map, a step-by-step solution (`steps`, `image`).
- Quiz for the family: question with numbered answers (`choices`), the result as `stats`.
- "Was kam heute in den Nachrichten?": headlines with pictures (`list`, `image`).

## Next

- `show_on_screen` on the Echo: kinds and blocks in the tool's schema (short, generic), web pages
  and videos; Jarvis' state (orb, captions) published by the speaker in the room automatically.
- Proactive cards from HA events (door bell, washing machine, alarms) with rules for when the TV
  may be interrupted.
- The idle screen as the stick's screensaver if Fire OS lets it.
- A photo slideshow block; QR codes (guest Wi-Fi).
