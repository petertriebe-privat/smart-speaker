package dev.jarvis.shared;

import android.content.Context;

// The HUD's link to the app that hosts it (the Fire TV app now, a phone app later, #75): the log
// tag and what happens when the full screen closes.
public final class Hud {
    public static final String TAG = "JarvisScreen";

    public interface Listener {
        // The full screen closed (back key, its time, a clear): e.g. a paused film plays on.
        void fullscreenClosed(Context ctx);
    }

    public static volatile Listener listener;

    private Hud() {
    }

    static void closed(Context ctx) {
        Listener l = listener;
        if (l != null) l.fullscreenClosed(ctx);
    }
}
