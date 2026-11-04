package dev.jarvis.shared;

import android.os.Handler;
import android.os.Looper;
import android.webkit.WebView;

// VoiceLevel hands the loudness of the voice Jarvis hears or speaks (AudioBridge.level) to a HUD
// page 20 times a second while it is shown: the hologram pulses with it as the Echo's ring does
// (setLevel in assets/jarvis.js). Silence is sent once, then nothing until there is sound again.
final class VoiceLevel implements Runnable {
    private static final long EVERY_MS = 50;
    private final Handler main = new Handler(Looper.getMainLooper());
    private final WebView page;
    private boolean running;
    private float sent;

    VoiceLevel(WebView page) {
        this.page = page;
    }

    void start() {
        if (running) return;
        running = true;
        main.postDelayed(this, EVERY_MS);
    }

    void stop() {
        running = false;
        main.removeCallbacks(this);
    }

    @Override
    public void run() {
        if (!running) return;
        float v = AudioBridge.level;
        AudioBridge.level = 0; // taken: the next chunks measure afresh
        if (v > 0.02f || sent > 0.02f) page.evaluateJavascript("setLevel(" + v + ")", null);
        sent = v;
        main.postDelayed(this, EVERY_MS);
    }
}
