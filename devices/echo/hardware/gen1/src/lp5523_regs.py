# Read (and optionally write) LP5523 registers over /dev/i2c-2 (Python 2.7, no ctypes).
# Usage: python lp5523_regs.py ADDR [REG=VALUE ...]   e.g. 0x32   or   0x32 0x00=0x40
import fcntl, os, sys

I2C_SLAVE_FORCE = 0x0706  # the kernel driver owns the address


def main():
    addr = int(sys.argv[1], 16)
    fd = os.open('/dev/i2c-2', os.O_RDWR)
    fcntl.ioctl(fd, I2C_SLAVE_FORCE, addr)
    for arg in sys.argv[2:]:
        reg, value = [int(x, 16) for x in arg.split('=')]
        os.write(fd, chr(reg) + chr(value))
    out = []
    for reg in list(range(0x00, 0x1f)) + list(range(0x36, 0x3c)) + list(range(0x70, 0x76)):
        os.write(fd, chr(reg))
        out.append('%02x=%02x' % (reg, ord(os.read(fd, 1))))
    print ' '.join(out)


main()
