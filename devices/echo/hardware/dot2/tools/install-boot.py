"""Install the reviewed own Linux bootstrap into the identified unlocked Dot.

Requires --install. Does not alter preloader, LK, GPT or Android system slots.
The first boot restores the hash-checked TWRP backup unless SSH is confirmed.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--project', type=Path, required=True)
    ap.add_argument('--install', action='store_true')
    args = ap.parse_args()
    assert args.install, 'Explicit --install required'
    p = args.project.resolve()
    stage = p / 'local/dot2-tools/dot-linux-stage'
    profile = json.loads((p / 'local/dot-usb/twrp-first-probe.json').read_text())
    verified = json.loads((p / 'local/dot-usb/unlock-verification.json').read_text())
    image = json.loads((stage / 'boot-image-manifest.json').read_text())
    backup = Path(verified['backup_directory']) / 'recovery.img'
    assert hashlib.sha256(backup.read_bytes()).hexdigest() == image['donor_recovery_sha256']
    assert hashlib.sha256(Path(image['image']).read_bytes()).hexdigest() == image['sha256']
    os.environ['ANDROID_USER_HOME'] = str(p / 'local/dot2-tools/android-user')
    prefix = [str(p / 'local/dot2-tools/platform-tools/adb.exe'), '-s', profile['serial']]

    def shell(command, timeout=30):
        r = subprocess.run(prefix + ['shell', command], capture_output=True, text=True, timeout=timeout)
        assert r.returncode == 0, f'Device command failed: {command.splitlines()[0]}\n{r.stderr}'
        return r.stdout.strip()

    assert shell('getprop ro.twrp.version') == '3.7.0_9-0'
    assert shell('getprop ro.product.device') == 'biscuit'
    assert 'uid=0(root)' in shell('id')
    assert shell('cat /sys/class/block/mmcblk0p12/size') == '32768'
    assert shell('sha256sum /dev/block/mmcblk0p12').split()[0] == image['donor_recovery_sha256']
    root = '/data/echo-dot-dev/rootfs'
    for name in ('init', 'network.sh', 'dhcp.sh', 'services.sh', 'to-twrp.sh', 'shutdown.sh'):
        target = '/tmp/dot-check-' + name
        subprocess.run(prefix + ['push', str(p / 'hardware/dot2/tools' / name), target], check=True, capture_output=True, timeout=20)
        shell(f'/sbin/sh -n {target}')
    assert 'init' in shell(f'chroot {root} /bin/busybox.static --list').splitlines()
    base = '/data/echo-dot-dev'
    shell(f'mkdir -p {base}/artifacts {base}/services {base}/bin')
    for local, target in [(backup, base + '/recovery-twrp.img'), (Path(image['image']), base + '/artifacts/bootstrap-1.img')]:
        subprocess.run(prefix + ['push', str(local), target], check=True, capture_output=True, timeout=60)
    assert shell(f'sha256sum {base}/recovery-twrp.img').split()[0] == image['donor_recovery_sha256']
    assert shell(f'sha256sum {base}/artifacts/bootstrap-1.img').split()[0] == image['sha256']
    checksum_file = stage / 'recovery-twrp.img.sha256'
    checksum_file.write_text(image['donor_recovery_sha256'] + '  recovery-twrp.img\n', encoding='ascii', newline='\n')
    subprocess.run(prefix + ['push', str(checksum_file), base + '/recovery-twrp.img.sha256'], check=True, capture_output=True, timeout=20)
    shell(f'touch {base}/boot-trial; sync')
    # TWRP's stripped-down dd rejects conv=fsync; flush explicitly before readback.
    shell(f'dd if={base}/artifacts/bootstrap-1.img of=/dev/block/mmcblk0p12 bs=65536 && sync', timeout=60)
    actual = shell(f'dd if=/dev/block/mmcblk0p12 bs={image["image_bytes"]} count=1 2>/dev/null | sha256sum', timeout=60).split()[0]
    assert actual == image['sha256'], 'Flashed recovery does not match built image'
    record = {'image_manifest': image, 'flashed_prefix_sha256': actual, 'trial_timeout_seconds': 240,
              'state': 'Image installed and verified; awaiting own Linux boot/SSH validation', 'time': time.time()}
    (p / 'local/dot-usb/linux-installation.json').write_text(json.dumps(record, indent=2), encoding='utf-8', newline='\n')
    print('Own recovery boot image written and hash verified. Rebooting into first Linux trial.', flush=True)
    subprocess.run(prefix + ['reboot', 'recovery'], check=True, capture_output=True, timeout=15)


if __name__ == '__main__':
    main()
