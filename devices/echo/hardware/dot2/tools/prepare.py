"""Build a private Dot network test rootfs from previously verified input archives.

No flashing. Run on the Windows development host with --project <Alexa path>.
The upstream sources are pinned and recorded under local/dot2-tools/techo5-review.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--project', type=Path, required=True)
    args = parser.parse_args()
    project = args.project.resolve()
    stage = project / 'local/dot2-tools/dot-linux-stage'
    sources = project / 'local/dot2-tools/techo5-review/source'
    manifest = json.loads((stage / 'inputs-manifest.json').read_text())
    sys.path.insert(0, str(sources / 'tools/linux'))
    from mkrootfs import Root

    ssh = project / 'local/dot-ssh'
    ssh.mkdir(exist_ok=True)
    key = ssh / 'id_ed25519'
    if not key.exists():
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-C', 'echo-dot-dev',
                        '-f', str(key), '-N', ''], check=True, capture_output=True)
    wifi = json.loads((project / 'local/wifi-settings.json').read_text(encoding='utf-8-sig'))
    ssid = wifi['ssid'].encode('utf-8')
    password = wifi['password']
    assert 1 <= len(ssid) <= 32
    assert 8 <= len(password) <= 63 or (len(password) == 64 and all(c in '0123456789abcdefABCDEF' for c in password))
    psk = password.lower() if len(password) == 64 else hashlib.pbkdf2_hmac('sha1', password.encode('utf-8'), ssid, 4096, 32).hex()
    country = wifi.get('country', 'DE')
    assert len(country) == 2 and country.isalpha()
    private = stage / 'private'
    private.mkdir(exist_ok=True)
    config = f'ctrl_interface=/run/wpa\nupdate_config=0\ncountry={country.upper()}\nnetwork={{\n ssid={ssid.hex()}\n psk={psk}\n key_mgmt=WPA-PSK\n}}\n'
    (private / 'wpa_supplicant.conf').write_text(config, encoding='utf-8', newline='\n')
    (private / 'authorized_keys').write_bytes(key.with_suffix('.pub').read_bytes())

    root = Root()
    root.add_tar(manifest['alpine_path'], skip_dotfiles=True)
    for apk in sorted((stage / 'apks').glob('*.apk')):
        root.add_tar(apk, skip_dotfiles=True)
    root.file('bin/busybox.static', (stage / 'busybox.static').read_bytes(), 0o755)
    root.file('usr/local/bin/wmtup', (stage / 'wmtup').read_bytes(), 0o755)
    tool_dir = project / 'hardware/dot2/tools'
    for source, destination in [('network.sh', 'usr/local/sbin/echo-dot-network'), ('dhcp.sh', 'usr/local/sbin/echo-dot-dhcp')]:
        root.file(destination, (tool_dir / source).read_bytes().replace(b'\r\n', b'\n'), 0o755)
    for directory in ('proc', 'sys', 'dev', 'dev/pts', 'run', 'tmp', 'data', 'system/vendor', 'root', 'root/.ssh'):
        root.dir(directory, 0o700 if directory.startswith('root') else 0o755)
    root.file('etc/hostname', b'echo-dot-dev\n')
    root.file('etc/motd', b'Echo Dot 2 development Linux. Full root.\n')
    root.finish(str(stage / 'network-rootfs.tar.gz'))
    print('Private Wi-Fi configuration, dedicated SSH key and network rootfs prepared; no secrets printed.')


if __name__ == '__main__':
    main()
