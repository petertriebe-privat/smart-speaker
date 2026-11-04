"""Build own SSH development initramfs; never flash from this builder."""
import argparse
import gzip
import hashlib
import json
from pathlib import Path
import sys


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--project', type=Path, required=True)
    p = ap.parse_args().project.resolve()
    stage = p / 'local/dot2-tools/dot-linux-stage'
    sources = p / 'local/dot2-tools/techo5-review/source'
    sys.path.insert(0, str(sources / 'tools/linux'))
    from mkimage import Cpio, read_bootimg, verify_cpio, write_bootimg
    from mkrootfs import Root
    manifest = json.loads((stage / 'inputs-manifest.json').read_text())
    verified = json.loads((p / 'local/dot-usb/unlock-verification.json').read_text())
    backup = Path(verified['backup_directory']) / 'recovery.img'
    backup_manifest = json.loads((backup.parent / 'manifest.json').read_text())
    expected = next(x['sha256'] for x in backup_manifest['files'] if x['name'] == 'recovery.img')
    assert hashlib.sha256(backup.read_bytes()).hexdigest() == expected

    root = Root()
    root.add_tar(str(stage / 'network-rootfs.tar.gz'))
    tool_dir = p / 'hardware/dot2/tools'
    scripts = {'init': 'init', 'network.sh': 'usr/local/sbin/echo-dot-network',
               'dhcp.sh': 'usr/local/sbin/echo-dot-dhcp', 'services.sh': 'usr/local/sbin/echo-dot-services',
               'to-twrp.sh': 'usr/local/sbin/echo-dot-to-twrp', 'shutdown.sh': 'usr/local/sbin/echo-dot-shutdown'}
    for name, destination in scripts.items():
        root.file(destination, (tool_dir / name).read_bytes().replace(b'\r\n', b'\n'), 0o755)
    inittab = ('::respawn:/usr/local/sbin/echo-dot-network\n'
               '::respawn:/usr/local/sbin/echo-dot-services\n'
               'ttyGS0::respawn:/bin/busybox.static sh -l\n'
               '::ctrlaltdel:/sbin/reboot\n'
               '::shutdown:/usr/local/sbin/echo-dot-shutdown\n')
    root.file('etc/inittab', inittab.encode())
    # Logical root stays in RAM; persistent code/configuration lives on userdata.
    root.file('etc/echo-dot-dev-release', b'echo-dot-dev bootstrap-2\n')
    cpio = Cpio()
    for name, (info, data) in root.entries.items():
        name = name.rstrip('/')
        if info.isdir():
            cpio.dir(name, info.mode)
        elif info.issym():
            cpio.symlink(name, info.linkname)
        elif info.isfile():
            cpio.file(name, data, info.mode)
    for name, major, minor in [('dev/console', 5, 1), ('dev/null', 1, 3), ('dev/kmsg', 1, 11)]:
        cpio.chardev(name, major, minor, 0o600)
    raw = cpio.finish()
    names = verify_cpio(raw)
    assert all(x in names for x in ['init', 'bin/busybox.static', 'usr/sbin/dropbear', 'sbin/wpa_supplicant', 'usr/local/bin/wmtup'])
    ramdisk = gzip.compress(raw, compresslevel=9, mtime=0)
    header, kernel, old_ramdisk, page_size = read_bootimg(str(backup))
    out = stage / 'echo-dot-dev.img'
    size = write_bootimg(header, kernel, ramdisk, page_size, str(out),
                         cmdline_append='androidboot.selinux=permissive enforcing=0',
                         cmdline_drop=('skip_initramfs', 'root=', 'dm='))
    assert size <= 16 * 1024 * 1024
    record = {'image': str(out), 'image_bytes': size, 'sha256': hashlib.sha256(out.read_bytes()).hexdigest(),
              'donor_recovery_sha256': expected, 'kernel_sha256': hashlib.sha256(kernel).hexdigest(),
              'ramdisk_bytes': len(ramdisk), 'reference_commit': manifest['reference_commit'],
              'kernel_reused': True, 'contains_credentials': False,
              'writes_planned': ['userdata /data/echo-dot-dev', 'recovery mmcblk0p12', 'misc first 32 bytes boot-recovery']}
    (stage / 'boot-image-manifest.json').write_text(json.dumps(record, indent=2), encoding='utf-8', newline='\n')
    print(f'Own boot image validated: {size} bytes; original kernel reused; no Wi-Fi secrets in image.')


if __name__ == '__main__':
    main()
