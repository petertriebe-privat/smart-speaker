package dev.jarvis.phone.car;

import android.content.Context;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import dev.jarvis.shared.Hud;

import java.io.File;

// The car's flight recorder (#75): while Android Auto is connected, the app's Android log (the car
// connection, whether Android Auto bound the car app, the phone's tools, audio, crashes) goes into
// files/run/car-logcat.txt, rotated at 8 MB with 3 older files, and on for a minute after the car
// is gone; Android's log buffer holds only an hour or two. Read at home over ADB:
// adb exec-out run-as dev.jarvis.phone cat files/run/car-logcat.txt. Other apps' lines (Android
// Auto, Spotify) it cannot have: READ_LOGS, granted over ADB, still asks the user on the screen
// each time since Android 13 and is refused in the background (06.11.2026).
public final class CarLog {
    private static final long AFTER_MS = 60_000;
    private static final Handler main = new Handler(Looper.getMainLooper());
    private static Process logcat;
    private static final Runnable stop = CarLog::end;

    private CarLog() {
    }

    // connected: Android Auto's connection type changed (CarConnection).
    public static synchronized void connected(Context ctx, int type, boolean projection) {
        Log.i(Hud.TAG, "car connection: type " + type + (projection ? " (Android Auto)" : ""));
        main.removeCallbacks(stop);
        if (!projection) {
            if (logcat != null) main.postDelayed(stop, AFTER_MS);
            return;
        }
        if (logcat != null) return;
        File dir = new File(ctx.getFilesDir(), "run");
        dir.mkdirs();
        try {
            logcat = new ProcessBuilder("logcat", "-v", "threadtime", "-b", "main,system,crash,events",
                    "-f", new File(dir, "car-logcat.txt").getPath(), "-r", "8192", "-n", "3")
                    .redirectErrorStream(true).start();
            Log.i(Hud.TAG, "car log: recording");
        } catch (Exception e) {
            Log.w(Hud.TAG, "car log: " + e);
        }
    }

    private static synchronized void end() {
        if (logcat == null) return;
        Log.i(Hud.TAG, "car log: stopped");
        logcat.destroy();
        logcat = null;
    }
}
