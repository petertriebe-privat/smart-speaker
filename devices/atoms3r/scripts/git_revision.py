# The commit the firmware is built from as GIT_REVISION ("abc1234", "+" when the tree has changes),
# for Home Assistant's diagnostics (services/ha_device, #103). "unknown" without git.
import subprocess

Import("env")  # noqa: F821 (PlatformIO)


def revision():
    try:
        rev = subprocess.check_output(["git", "rev-parse", "--short=7", "HEAD"], text=True).strip()
        dirty = subprocess.call(["git", "diff", "--quiet", "HEAD", "--", "."]) != 0
        return rev + ("+" if dirty else "")
    except Exception:
        return "unknown"


env.Append(CPPDEFINES=[("GIT_REVISION", '\\"%s\\"' % revision())])  # noqa: F821
