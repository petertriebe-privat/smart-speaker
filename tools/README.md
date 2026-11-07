# Tools

Tools for more than one device, run on the PC from the repository root. Each reads the
household's values from the git-ignored `local/` ([local/README.md](../local/README.md)) and
never prints them. Tools for one device live with it: `core/tools/` (prompts.json, secrets.json,
test speech), `devices/echo/tools/` (deploy), `devices/android/tools/` (install),
`devices/atoms3r/scripts/` and `devices/atoms3r/models/` (build steps).

| Tool | What | Run |
|---|---|---|
| [wakeword/](wakeword/README.md) | Trains an own wake word model for the Echos and the M5 | `tools/wakeword/pipeline.sh` (Docker) |
| `google_auth.py` | One-time Google OAuth consent; writes the refresh token into `local/secrets.h` | `py -3 tools/google_auth.py` |
| `spotify_auth.py` | One-time Spotify OAuth consent; `--other` for another person's account | `py -3 tools/spotify_auth.py` |
| `openai_costs.py` | What OpenAI billed in the last days, by project, line item and day | `py -3 tools/openai_costs.py --days 7` |
| `migrate_archive.py` | Moves the memory archive from the mailbox to Google Drive (one-time; safe to run again) | `py -3 tools/migrate_archive.py --dry-run` |
| `github-settings.sh` | The repository's settings on GitHub: ruleset on main, fork approval, security features | `tools/github-settings.sh <owner>/<repo>` |

After a new token in `local/secrets.h`: `py -3 core/tools/make_secrets.py`, then deploy the
secrets (`devices/echo/tools/deploy.sh --all --secrets`, `devices/android/tools/install.sh`).
