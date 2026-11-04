# local/: the household's own values

Everything that belongs to one household and is used by more than one device lives here, in one
place. The folder is git-ignored except for this file and the `*.example` templates: nothing in
it is ever committed, and the code takes every household value from here, never hard-wired.

| File | What | Read by |
|---|---|---|
| `secrets.h` | WiFi, API keys, Home Assistant, accounts, people, mailboxes, `ASSISTANT_NAME`. Start from `secrets.h.example`. | The M5's build (`-I local`), `core/tools/make_secrets.py`, the tools in `tools/`, the M5's test suite |
| `secrets.json` | The same values as JSON, made by `py -3 core/tools/make_secrets.py` after every change to `secrets.h` | The Echos (`devices/echo/tools/deploy.sh --secrets`), the Fire TV and the phone (`devices/android/tools/install*.sh`) |
| `persona_local.h` | Optional: another character than Jarvis, the constants of `shared/prompts/persona.h` | The M5's build instead of the default |
| `persona.json` | That character for the Echos, made by `py -3 core/tools/extract_prompts.py --persona` | The Echo's state directory |
| `wakeword/` | The household's wake word: `recordings/` (the voices) and the trained `jarvis.tflite` + `jarvis.json` (`tools/wakeword`) | Training, the Echos (`wake_model`), the phone (`install_phone.sh`) |
| `wake_model_own.h` | The same model as a header for the M5 (`devices/atoms3r/models/make_model_header.py`) | The M5's build instead of "Hey Jarvis" |
| `home-gateway/` | `gateway.env`, the CA and the device certificates for the way home (`services/home-gateway`) | `services/home-gateway/install.sh`, `install_phone.sh` |

What belongs to one device only stays with it: the Echos' SSH profiles, per-device files
(`devices/<host>.json`) and builds in `devices/echo/local/`.

## Setting up

```sh
cp local/secrets.h.example local/secrets.h   # then fill it in
py -3 core/tools/make_secrets.py             # local/secrets.json for the Echos, Fire TV and phone
```

`tools/google_auth.py` and `tools/spotify_auth.py` write their tokens into `local/secrets.h`;
run `make_secrets.py` again afterwards and deploy the secrets.

## A worktree

A second checkout (`git worktree`) has an empty `local/`. Copy the folder from the main checkout,
or point the install scripts at it: `JARVIS_SECRETS`, `HOME_GATEWAY_LOCAL`, `JARVIS_WAKEMODEL`.
