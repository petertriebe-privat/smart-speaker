"""Show the sample cards (tools/samples.json) on a Jarvis screen, one after the other.

Usage: python devices/android/tools/demo.py <room id> [sample name ...]
Needs paho-mqtt (pip install paho-mqtt) and the broker from local/secrets.json.
Each card stays as long as its "show" seconds; the overlay cards appear above whatever runs.
"""
import json
import os
import sys
import time
import urllib.parse

import paho.mqtt.client as mqtt

HERE = os.path.dirname(os.path.abspath(__file__))
SECRETS = os.path.join(HERE, "..", "..", "echo", "local", "assistant", "secrets.json")


def main():
    room, wanted = sys.argv[1], set(sys.argv[2:])
    s = json.load(open(SECRETS, encoding="utf-8"))
    u = urllib.parse.urlparse(s["MQTT_URL"])
    c = mqtt.Client(mqtt.CallbackAPIVersion.VERSION2, client_id="jarvis-screen-demo")
    c.username_pw_set(s.get("MQTT_USER"), s.get("MQTT_PASSWORD"))
    c.connect(u.hostname, u.port or 1883)
    c.loop_start()
    base = "jarvis/screen/" + room
    samples = json.load(open(os.path.join(HERE, "samples.json"), encoding="utf-8"))
    for sample in samples:
        if wanted and sample["name"] not in wanted:
            continue
        card = dict(sample["card"], id="demo-%d" % time.time(), seconds=sample["show"])
        print(sample["name"], flush=True)
        c.publish(base + "/show", json.dumps(card))  # the app swaps overlay and full screen itself
        time.sleep(sample["show"] + 1)
    c.publish(base + "/clear", b"")
    time.sleep(0.5)
    c.loop_stop()


if __name__ == "__main__":
    main()
