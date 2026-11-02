"""Install and test the own network rootfs inside the running Dot TWRP; no flashing."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import time


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--project', type=Path, required=True)
    project = ap.parse_args().project.resolve()
    stage = project / 'local/dot2-tools/dot-linux-stage'
    os.environ['ANDROID_USER_HOME'] = str(project / 'local/dot2-tools/android-user')
    adb = str(project / 'local/dot2-tools/platform-tools/adb.exe')
    profile = json.loads((project / 'local/dot-usb/twrp-first-probe.json').read_text())
    prefix = [adb, '-s', profile['serial']]

    def shell(command, timeout=30):
        r = subprocess.run(prefix + ['shell', command], capture_output=True, text=True, timeout=timeout)
        if r.returncode:
            raise RuntimeError(f'Device command failed: {command.splitlines()[0]}\n{r.stderr}')
        return r.stdout

    assert '3.7.0_9-0' in shell('getprop ro.twrp.version')
    assert 'uid=0(root)' in shell('id')
    base = '/data/echo-dot-dev'
    root = base + '/rootfs'
    shell(f'mkdir -p {base}/private; chmod 700 {base} {base}/private')
    for local, target in [(stage / 'network-rootfs.tar.gz', '/tmp/dot-network-rootfs.tar.gz'),
                          (stage / 'private/wpa_supplicant.conf', base + '/private/wpa_supplicant.conf'),
                          (stage / 'private/authorized_keys', base + '/private/authorized_keys')]:
        subprocess.run(prefix + ['push', str(local), target], check=True, capture_output=True, timeout=60)
    command = f'''set -e
chmod 600 {base}/private/*
mkdir -p {root}
tar -xzf /tmp/dot-network-rootfs.tar.gz -C {root}
mkdir -p {base}/firmware
cp /tmp/dot-system-b/system/vendor/firmware/* {base}/firmware/
test -s {base}/firmware/WMT_SOC.cfg
test -s {base}/firmware/WIFI_RAM_CODE_8163
mkdir -p /system_root/system/vendor/firmware
mount --bind {base}/firmware /system_root/system/vendor/firmware
ln -s /system/vendor /vendor
mount --bind /dev {root}/dev
mount --bind /dev/pts {root}/dev/pts
mount --bind /proc {root}/proc
mount --bind /sys {root}/sys
mount --bind /data {root}/data
mkdir -p {root}/run/echo-dot-dev
chroot {root} /bin/sh -n /usr/local/sbin/echo-dot-network
chroot {root} /bin/sh -n /usr/local/sbin/echo-dot-dhcp
chroot {root} /usr/sbin/dropbear -V
chroot {root} /sbin/wpa_supplicant -v
'''
    versions = shell(command, timeout=60)
    print('Rootfs extracted and runtime dependencies validated.', flush=True)
    subprocess.run(prefix + ['shell', f'nohup chroot {root} /usr/local/sbin/echo-dot-network > {base}/network-stage.log 2>&1 < /dev/null &'], capture_output=True, timeout=15, check=True)
    for _ in range(35):
        time.sleep(2)
        info = shell(f'ls /sys/class/net; pidof wmtup; ip -4 -o addr show wlan0 2>/dev/null; tail -n 4 {root}/run/echo-dot-dev/wmtup.log; tail -n 3 {root}/run/echo-dot-dev/ssh.log', timeout=15)
        if 'inet ' in info:
            break
    record = {'mode': 'TWRP chroot test; boot not changed', 'versions': versions, 'result': info}
    (project / 'local/dot-usb/network-stage-result.json').write_text(json.dumps(record, indent=2), encoding='utf-8')
    print('WLAN has IPv4 address:', 'inet ' in info, flush=True)
    if 'inet ' not in info:
        print(info.split('wlan0')[0], flush=True)


if __name__ == '__main__':
    main()
