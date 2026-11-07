# The commit the firmware is built from as GIT_REVISION ("abc1234", "+" when the tree has changes),
# for Home Assistant's diagnostics (services/ha_device, #103). "unknown" without git.
# The release as FIRMWARE_VERSION (issue #15): ASSISTANT_VERSION=v1.2.0 in the environment of the
# owner's private build (the firmware holds secrets.h, so it is never built on GitHub); "dev"
# for any other build.
import os
import re
import subprocess

Import("env")  # noqa: F821 (PlatformIO)


def revision():
    try:
        rev = subprocess.check_output(["git", "rev-parse", "--short=7", "HEAD"], text=True).strip()
        dirty = subprocess.call(["git", "diff", "--quiet", "HEAD", "--", "."]) != 0
        return rev + ("+" if dirty else "")
    except Exception:
        return "unknown"


def version():
    v = os.environ.get("ASSISTANT_VERSION", "")
    if not v:
        return "dev"
    if not re.fullmatch(r"v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-dev\.[1-9]\d*)?", v):
        raise SystemExit(f"ASSISTANT_VERSION: not a release version: {v}")
    return v


env.Append(CPPDEFINES=[("GIT_REVISION", '\\"%s\\"' % revision()),  # noqa: F821
                       ("FIRMWARE_VERSION", '\\"%s\\"' % version())])
