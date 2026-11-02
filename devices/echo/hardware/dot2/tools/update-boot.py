"""Update only the own Dot Linux recovery image over verified SSH.

The installed image must match the previous installation record. The first boot
of the update is a 240-second trial with a verified TWRP rollback copy on userdata.
"""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import time


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--project', type=Path, required=True)
    ap.add_argument('--install', action='store_true')
    args = ap.parse_args()
    assert args.install
    p = args.project.resolve()
    cfg = p / 'local/dot-ssh/config'
    stage = p / 'local/dot2-tools/dot-linux-stage'
    image = json.loads((stage / 'boot-image-manifest.json').read_text())
    old_path = p / 'local/dot-usb/linux-installation.json'
    old = json.loads(old_path.read_text())
    payload = Path(image['image']).read_bytes()
    assert hashlib.sha256(payload).hexdigest() == image['sha256']

    def shell(command, data=None, timeout=30):
        r = subprocess.run(['ssh', '-F', str(cfg), 'dot-dev', command], input=data,
                           capture_output=True, timeout=timeout)
        assert r.returncode == 0, 'Dot SSH command failed: ' + r.stderr.decode(errors='replace')
        return r.stdout.decode().strip()

    assert 'uid=0(root)' in shell('test -f /etc/echo-dot-dev-release && id')
    assert shell('cat /sys/class/block/mmcblk0p12/size') == '32768'
    installed = shell(f'dd if=/dev/mmcblk0p12 bs={old["image_manifest"]["image_bytes"]} count=1 2>/dev/null | sha256sum').split()[0]
    assert installed == old['image_manifest']['sha256'], 'Unexpected installed recovery image'
    shell('cd /data/echo-dot-dev && sha256sum -c recovery-twrp.img.sha256')
    target = '/data/echo-dot-dev/artifacts/bootstrap-2.img'
    shell('umask 077; cat > ' + target, payload, timeout=60)
    assert shell('sha256sum ' + target).split()[0] == image['sha256']
    shell(f'touch /data/echo-dot-dev/boot-trial; sync; dd if={target} of=/dev/mmcblk0p12 bs=65536 conv=fsync; sync', timeout=60)
    actual = shell(f'dd if=/dev/mmcblk0p12 bs={image["image_bytes"]} count=1 2>/dev/null | sha256sum').split()[0]
    assert actual == image['sha256']
    (p / 'local/dot-usb/linux-installation-bootstrap-1.json').write_text(json.dumps(old, indent=2), encoding='utf-8')
    record = {'image_manifest': image, 'flashed_prefix_sha256': actual,
              'trial_timeout_seconds': 240, 'state': 'Awaiting updated own Linux boot', 'time': time.time()}
    old_path.write_text(json.dumps(record, indent=2), encoding='utf-8', newline='\n')
    print('Updated Linux image verified; rebooting.', flush=True)
    # BusyBox init performs the sync/unmount shutdown path; SSH may disconnect normally.
    subprocess.run(['ssh', '-F', str(cfg), 'dot-dev', 'reboot'], capture_output=True, timeout=15)


if __name__ == '__main__':
    main()
