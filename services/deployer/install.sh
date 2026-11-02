#!/bin/sh
# Install or update the deploy on the Raspberry Pi (run there with sudo from a checkout of the
# repository). See README.md.
#   sudo REPO=<owner>/<name> RUNNER_TOKEN=<token> sh services/deployer/install.sh
# RUNNER_TOKEN only for the first run (a registration token, valid one hour:
# gh api -X POST repos/<owner>/<name>/actions/runners/registration-token --jq .token).
# Creates the user assistant-deploy, /opt/assistant-deployer with deployer.py, the
# environment file to fill in, and GitHub's self-hosted runner (label assistant-deploy) as a service.
set -eu
SRC=$(cd "$(dirname "$0")" && pwd)
HOME_DIR=/opt/assistant-deployer
ENV=/etc/assistant-deployer.env
id assistant-deploy >/dev/null 2>&1 || useradd --system --create-home --home-dir "$HOME_DIR" --shell /bin/sh assistant-deploy
mkdir -p "$HOME_DIR/builds" "$HOME_DIR/local/devices" "$HOME_DIR/local/firetv" "$HOME_DIR/.ssh" "$HOME_DIR/.android"
# the copies the CI runs: a commit cannot change them
install -m 755 "$SRC/deployer.py" "$HOME_DIR/deployer.py"
install -m 755 "$SRC/firetv_deploy.py" "$HOME_DIR/firetv_deploy.py"
# The Fire TV sticks (#126): ADB and the signing tool; the PC's ~/.android/adbkey(.pub) and
# debug.keystore go to $HOME_DIR/.android by hand (README.md).
command -v adb >/dev/null && command -v apksigner >/dev/null || apt-get install -y -qq adb apksigner >/dev/null
if [ ! -f "$ENV" ]; then
    printf '%s\n' "# The assistant's Telegram bot (sending only) and the owner's chat" \
        "TELEGRAM_BOT_TOKEN=" "TELEGRAM_CHAT_ID=" >"$ENV"
fi
chown root:assistant-deploy "$ENV"
chmod 640 "$ENV"
if [ ! -x "$HOME_DIR/runner/run.sh" ]; then
    : "${REPO:?REPO=<owner>/<name> for the first run}" "${RUNNER_TOKEN:?RUNNER_TOKEN for the first run}"
    v=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p')
    mkdir -p "$HOME_DIR/runner"
    curl -fsSL "https://github.com/actions/runner/releases/download/v$v/actions-runner-linux-arm64-$v.tar.gz" |
        tar -xz -C "$HOME_DIR/runner"
    chown -R assistant-deploy:assistant-deploy "$HOME_DIR"
    "$HOME_DIR/runner/bin/installdependencies.sh" >/dev/null
    sudo -u assistant-deploy sh -c "cd $HOME_DIR/runner && ./config.sh --unattended --replace \
        --url https://github.com/$REPO --token $RUNNER_TOKEN --name assistant-pi --labels assistant-deploy"
    (cd "$HOME_DIR/runner" && ./svc.sh install assistant-deploy && ./svc.sh start)
fi
chown -R assistant-deploy:assistant-deploy "$HOME_DIR"
chmod 700 "$HOME_DIR/.ssh"
# The server agent's read-only diagnostics of the Echos (#124): AGENT_KEY=<public key> once; the key may only
# run assistant-diag, which may only run the diagnostics as assistant-deploy.
mkdir -p /usr/local/lib/assistant-diag
install -m 755 "$SRC/assistant_diag.py" /usr/local/lib/assistant-diag/assistant_diag.py
install -m 755 "$SRC/assistant-diag" /usr/local/bin/assistant-diag
if [ -n "${AGENT_KEY:-}" ]; then
    id agent >/dev/null 2>&1 || useradd --system --create-home --home-dir /home/agent --shell /bin/sh agent
    mkdir -p /home/agent/.ssh
    echo "restrict,command=\"/usr/local/bin/assistant-diag\" $AGENT_KEY" >/home/agent/.ssh/authorized_keys
    chown -R agent:agent /home/agent/.ssh
    chmod 700 /home/agent/.ssh
    chmod 600 /home/agent/.ssh/authorized_keys
    rule="agent ALL=(assistant-deploy) NOPASSWD: /usr/bin/python3 /usr/local/lib/assistant-diag/assistant_diag.py *"
    echo "$rule" >/etc/sudoers.d/assistant-diag.new
    chmod 440 /etc/sudoers.d/assistant-diag.new
    visudo -cf /etc/sudoers.d/assistant-diag.new >/dev/null && mv /etc/sudoers.d/assistant-diag.new /etc/sudoers.d/assistant-diag
fi
echo "then: the PC's speaker keys and $HOME_DIR/.ssh/config, $HOME_DIR/local/devices/*.json, $ENV (README.md)"
