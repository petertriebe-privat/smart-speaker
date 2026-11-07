#!/bin/sh
# Install or update the deploy on the Raspberry Pi (run there with sudo from a checkout of the
# repository). See README.md.
#   sudo REPO=<owner>/smart-speaker-deploy RUNNER_TOKEN=<token> sh services/deployer/install.sh
# REPO is the private deploy repository: the runner refuses a repository anyone can read.
# RUNNER_TOKEN only for the first run (a registration token, valid one hour:
# gh api -X POST repos/<owner>/smart-speaker-deploy/actions/runners/registration-token --jq .token).
# Creates the user jarvis-deploy, /opt/jarvis-deployer with deployer.py, the
# environment file to fill in, and GitHub's self-hosted runner (label jarvis-deploy) as a service.
set -eu
SRC=$(cd "$(dirname "$0")" && pwd)
HOME_DIR=/opt/jarvis-deployer
ENV=/etc/jarvis-deployer.env
id jarvis-deploy >/dev/null 2>&1 || useradd --system --create-home --home-dir "$HOME_DIR" --shell /bin/sh jarvis-deploy
mkdir -p "$HOME_DIR/builds" "$HOME_DIR/local/devices" "$HOME_DIR/local/firetv" "$HOME_DIR/.ssh" "$HOME_DIR/.android"
# the copies the CI runs: a commit cannot change them
install -m 755 "$SRC/deployer.py" "$HOME_DIR/deployer.py"
install -m 755 "$SRC/firetv_deploy.py" "$HOME_DIR/firetv_deploy.py"
# The Fire TV sticks (#126): ADB and the signing tool; the PC's ~/.android/adbkey(.pub) and
# debug.keystore go to $HOME_DIR/.android by hand (README.md).
command -v adb >/dev/null && command -v apksigner >/dev/null || apt-get install -y -qq adb apksigner >/dev/null
if [ ! -f "$ENV" ]; then
    printf '%s\n' "# Jarvis' Telegram bot (sending only) and the owner's chat" \
        "TELEGRAM_BOT_TOKEN=" "TELEGRAM_CHAT_ID=" >"$ENV"
fi
chown root:jarvis-deploy "$ENV"
chmod 640 "$ENV"
if [ ! -x "$HOME_DIR/runner/run.sh" ]; then
    : "${REPO:?REPO=<owner>/smart-speaker-deploy for the first run}" "${RUNNER_TOKEN:?RUNNER_TOKEN for the first run}"
    # a public repository's pull requests could run their own workflow on this runner
    if curl -fsS -o /dev/null "https://api.github.com/repos/$REPO"; then
        echo "$REPO can be read without login (public): the runner goes only to a private repository" >&2
        exit 1
    fi
    v=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
    mkdir -p "$HOME_DIR/runner"
    curl -fsSL "https://github.com/actions/runner/releases/download/v$v/actions-runner-linux-arm64-$v.tar.gz" |
        tar -xz -C "$HOME_DIR/runner"
    chown -R jarvis-deploy:jarvis-deploy "$HOME_DIR"
    "$HOME_DIR/runner/bin/installdependencies.sh" >/dev/null
    sudo -u jarvis-deploy sh -c "cd $HOME_DIR/runner && ./config.sh --unattended --replace \
        --url https://github.com/$REPO --token $RUNNER_TOKEN --name jarvis-pi --labels jarvis-deploy"
    (cd "$HOME_DIR/runner" && ./svc.sh install jarvis-deploy && ./svc.sh start)
fi
chown -R jarvis-deploy:jarvis-deploy "$HOME_DIR"
chmod 700 "$HOME_DIR/.ssh"
# Ultron's read-only diagnostics of the Echos (#124): ULTRON_KEY=<public key> once; the key may only
# run jarvis-diag, which may only run the diagnostics as jarvis-deploy.
mkdir -p /usr/local/lib/jarvis-diag
install -m 755 "$SRC/jarvis_diag.py" /usr/local/lib/jarvis-diag/jarvis_diag.py
install -m 755 "$SRC/jarvis-diag" /usr/local/bin/jarvis-diag
if [ -n "${ULTRON_KEY:-}" ]; then
    id ultron >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/ultron --shell /bin/sh ultron
    mkdir -p /home/ultron/.ssh
    echo "restrict,command=\"/usr/local/bin/jarvis-diag\" $ULTRON_KEY" >/home/ultron/.ssh/authorized_keys
    chown -R ultron:ultron /home/ultron/.ssh
    chmod 700 /home/ultron/.ssh
    chmod 600 /home/ultron/.ssh/authorized_keys
    rule="ultron ALL=(jarvis-deploy) NOPASSWD: /usr/bin/python3 /usr/local/lib/jarvis-diag/jarvis_diag.py *"
    echo "$rule" >/etc/sudoers.d/jarvis-diag.new
    chmod 440 /etc/sudoers.d/jarvis-diag.new
    visudo -cf /etc/sudoers.d/jarvis-diag.new >/dev/null && mv /etc/sudoers.d/jarvis-diag.new /etc/sudoers.d/jarvis-diag
fi
echo "then: the PC's speaker keys and $HOME_DIR/.ssh/config, $HOME_DIR/local/devices/*.json, $ENV (README.md)"
