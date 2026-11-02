#!/usr/bin/env python
# Pairing agent for the trial on the Echo (issue #51), BlueZ 4 on python 2.7 with dbus.
#
# Makes the adapter visible for a while and confirms a pairing with Secure Simple Pairing's
# numeric comparison: both sides show the same six digits, and only if they match is there
# nobody in between. The agent registers with the capability DisplayYesNo, so BlueZ asks it with
# RequestConfirmation; here that question goes to the console. In the finished feature the assistant
# says the number at the speaker and listens for the answer - that is the only difference.
#
#   python bt-agent.py                  120 s visible, ask on the console
#   python bt-agent.py --seconds 300
#   python bt-agent.py --name the assistant    the name the phone shows (runtime only, main.conf stays)
#   python bt-agent.py --yes            confirm without asking (only with nobody around, and
#                                       never outside a window you opened yourself)
#
# Outside such a window nothing may pair: the agent is gone, and BlueZ refuses. Run it only
# after bt-try.sh daemon, and keep the dead man's switch armed while you do.
from __future__ import print_function

import optparse  # Amazon's python 2.7.2 has no argparse
import sys

import dbus
import dbus.mainloop.glib
import dbus.service
import gobject

AGENT_PATH = "/assistant/agent"
IFACE = "org.bluez.Agent"


class Agent(dbus.service.Object):
    """What BlueZ asks while a device pairs."""

    def __init__(self, bus, path, always_yes):
        dbus.service.Object.__init__(self, bus, path)
        self.bus = bus
        self.always_yes = always_yes
        self.paired = None

    def ask(self, question):
        if self.always_yes:
            print("%s -> yes (--yes)" % question)
            return True
        sys.stdout.write("%s [y/N] " % question)
        sys.stdout.flush()
        try:
            answer = sys.stdin.readline().strip().lower()
        except KeyboardInterrupt:
            return False
        return answer in ("y", "yes", "j", "ja")

    def trust(self, device):
        """Let the phone reconnect on its own from now on."""
        try:
            dbus.Interface(self.bus.get_object("org.bluez", device),
                           "org.bluez.Device").SetProperty("Trusted", True)
        except dbus.DBusException as err:
            print("could not set Trusted: %s" % err)

    @dbus.service.method(IFACE, in_signature="", out_signature="")
    def Release(self):
        print("agent released")

    @dbus.service.method(IFACE, in_signature="ou", out_signature="")
    def RequestConfirmation(self, device, passkey):
        shown = "%06u" % passkey
        print("")
        print("  the phone should show:  %s %s" % (shown[:3], shown[3:]))
        print("  device: %s" % device)
        if not self.ask("  do the numbers match?"):
            raise dbus.DBusException("org.bluez.Error.Rejected")
        self.trust(device)
        self.paired = device
        print("  paired.")

    @dbus.service.method(IFACE, in_signature="o", out_signature="u")
    def RequestPasskey(self, device):
        """An old phone wants a number typed in: we name one."""
        passkey = 0
        sys.stdout.write("  passkey for %s (6 digits): " % device)
        sys.stdout.flush()
        try:
            passkey = int(sys.stdin.readline().strip())
        except ValueError:
            raise dbus.DBusException("org.bluez.Error.Rejected")
        return dbus.UInt32(passkey)

    @dbus.service.method(IFACE, in_signature="o", out_signature="s")
    def RequestPinCode(self, device):
        sys.stdout.write("  PIN for %s: " % device)
        sys.stdout.flush()
        pin = sys.stdin.readline().strip()
        if not pin:
            raise dbus.DBusException("org.bluez.Error.Rejected")
        return pin

    @dbus.service.method(IFACE, in_signature="ou", out_signature="")
    def DisplayPasskey(self, device, passkey):
        print("  type this on the phone: %06u" % passkey)

    @dbus.service.method(IFACE, in_signature="os", out_signature="")
    def Authorize(self, device, uuid):
        """A paired device wants a service (A2DP, HFP). Known device, so yes."""
        print("  %s wants %s" % (device, uuid))

    @dbus.service.method(IFACE, in_signature="s", out_signature="")
    def ConfirmModeChange(self, mode):
        print("  mode change: %s" % mode)

    @dbus.service.method(IFACE, in_signature="", out_signature="")
    def Cancel(self):
        print("  the other side cancelled")


def main():
    parser = optparse.OptionParser(description="pairing agent for the trial (BlueZ 4)")
    parser.add_option("--seconds", type="int", default=120, help="how long the Echo stays visible")
    parser.add_option("--name", default="Jarvis", help="the name the phone shows")
    parser.add_option("--yes", action="store_true", default=False, help="confirm without asking")
    args, _ = parser.parse_args()

    dbus.mainloop.glib.DBusGMainLoop(set_as_default=True)
    bus = dbus.SystemBus()
    manager = dbus.Interface(bus.get_object("org.bluez", "/"), "org.bluez.Manager")
    path = manager.DefaultAdapter()
    adapter = dbus.Interface(bus.get_object("org.bluez", path), "org.bluez.Adapter")

    adapter.SetProperty("Powered", True)
    adapter.SetProperty("Name", args.name)
    adapter.SetProperty("Pairable", True)
    adapter.SetProperty("DiscoverableTimeout", dbus.UInt32(args.seconds))
    adapter.SetProperty("Discoverable", True)

    agent = Agent(bus, AGENT_PATH, args.yes)
    adapter.RegisterAgent(AGENT_PATH, "DisplayYesNo")
    print("visible as %s for %d s - pair from the phone now" % (args.name, args.seconds))

    loop = gobject.MainLoop()
    gobject.timeout_add_seconds(args.seconds, loop.quit)
    try:
        loop.run()
    except KeyboardInterrupt:
        pass
    finally:
        try:
            adapter.SetProperty("Discoverable", False)
            adapter.SetProperty("Pairable", False)
            adapter.UnregisterAgent(AGENT_PATH)
        except dbus.DBusException as err:
            print("while closing: %s" % err)
    print("window closed; paired: %s" % (agent.paired or "nothing"))


if __name__ == "__main__":
    main()
