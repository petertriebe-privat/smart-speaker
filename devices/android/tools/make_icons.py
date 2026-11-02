"""Make the Fire TV app's banner (640 x 360) and icon (192 x 192) from a render of the assistant's 3D
hologram (tools/avatar.png: a screenshot of the console, cut to the sphere).

Usage: python devices/android/tools/make_icons.py   (needs Pillow; writes firetv/src/main/res/drawable-xhdpi)
A new render: open the console on the stick, `screencap -p`, cut a square around the sphere into
tools/avatar.png.
"""
import math
import os

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "firetv", "src", "main", "res", "drawable-xhdpi")
AVATAR = os.path.join(HERE, "avatar.png")
FONT = os.path.join(HERE, "..", "shared", "src", "main", "assets", "fonts", "Rajdhani-Bold.ttf")
AMBER, MUTED, BG = (255, 154, 31), (196, 173, 148), (5, 6, 8)


def avatar(size):
    """The render, scaled, its edge fading out (the console's grid and glow end softly)."""
    img = Image.open(AVATAR).convert("RGBA").resize((size, size), Image.LANCZOS)
    mask = Image.new("L", (size, size))
    c = (size - 1) / 2
    mask.putdata([max(0, min(255, int(255 * (1 - (math.hypot(x - c, y - c) / c - 0.82) / 0.18))))
                  for y in range(size) for x in range(size)])
    img.putalpha(mask)
    return img


def main():
    os.makedirs(OUT, exist_ok=True)
    # Fire TV draws the banner at 320 x 180 dp: twice that in pixels on the stick (xhdpi).
    banner = Image.new("RGBA", (640, 360), BG + (255,))
    banner.alpha_composite(avatar(360), (10, 0))
    d = ImageDraw.Draw(banner)
    d.text((372, 116), "JARVIS", font=ImageFont.truetype(FONT, 84), fill=AMBER)
    d.line([376, 222, 600, 222], fill=AMBER + (160,), width=3)
    d.text((376, 232), "Assistent", font=ImageFont.truetype(FONT, 38), fill=MUTED)
    banner.convert("RGB").save(os.path.join(OUT, "banner.png"))
    icon = Image.new("RGBA", (192, 192), BG + (255,))
    icon.alpha_composite(avatar(192))
    icon.convert("RGB").save(os.path.join(OUT, "icon.png"))
    print("banner.png, icon.png ->", os.path.normpath(OUT))


if __name__ == "__main__":
    main()
