package dev.jarvis.screen;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.LinkProperties;
import android.provider.Settings;
import android.util.Log;

import dev.jarvis.shared.AudioBridge;
import dev.jarvis.shared.CoreProcess;

import org.json.JSONObject;

import java.io.File;
import java.io.FileOutputStream;
import java.net.InetAddress;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

// The stick as a Jarvis of its own: the core (core/, board firetv) runs next to the screen, the
// remote's microphone and the TV's speaker through the AudioBridge. On only when the install
// script left the household's secrets in the app's files (install.sh --core); without them the
// app is the screen of the other Jarvis devices only.
final class Core {
    private final Context ctx;
    private final Config cfg;
    private AudioBridge audio;
    private CoreProcess process;

    Core(Context ctx, Config cfg) {
        this.ctx = ctx;
        this.cfg = cfg;
    }

    boolean configured() {
        return new File(ctx.getFilesDir(), "secrets.json").exists() && CoreProcess.binary(ctx).exists();
    }

    void start() {
        if (!configured()) {
            Log.i(ScreenService.TAG, "core off (no secrets.json: install.sh --core)");
            return;
        }
        File files = ctx.getFilesDir();
        try {
            writeSettings(files);
            audio = new AudioBridge();
            audio.start();
        } catch (Exception e) {
            Log.e(ScreenService.TAG, "core: " + e);
            return;
        }
        List<String> args = new ArrayList<>(Arrays.asList(CoreProcess.binary(ctx).getPath(),
                "-secrets", new File(files, "secrets.json").getPath(),
                "-settings", new File(files, "settings.json").getPath(),
                "-ctl", new File(files, "run/ctl.sock").getPath()));
        Map<String, String> env = new HashMap<>();
        env.put("JARVIS_BOARD", "firetv");
        env.put("JARVIS_AUDIO", "127.0.0.1:" + AudioBridge.PORT);
        env.put("JARVIS_ID", Settings.Secure.getString(ctx.getContentResolver(), Settings.Secure.ANDROID_ID));
        env.put("JARVIS_PACKAGE", ctx.getPackageName()); // repaired over HA's ADB (tvkeep.go)
        env.put("JARVIS_DNS", dnsServers());
        env.put("HOME", files.getPath());
        env.put("TMPDIR", ctx.getCacheDir().getPath());
        process = new CoreProcess(ctx, args, env);
        process.start();
    }

    void stop() {
        if (process != null) process.stop();
        if (audio != null) audio.stop();
    }

    // settings.json for the core, rewritten at every start; device.json next to it (from the
    // install script) may override any of it, as on the Echos.
    private void writeSettings(File files) throws Exception {
        new File(files, "state").mkdirs();
        new File(files, "run").mkdirs();
        JSONObject s = new JSONObject();
        s.put("state_dir", new File(files, "state").getPath());
        s.put("run_dir", new File(files, "run").getPath());
        s.put("room", cfg.room);
        s.put("name", cfg.name.isEmpty() ? "Jarvis TV " + cfg.room : cfg.name); // apart from the room's Echo
        s.put("volume", 100);          // the TV's own volume rules
        s.put("wake_word", false);     // the remote's microphone key instead
        s.put("echo_gate", false);
        s.put("music_ring", false);
        s.put("spotify_name", "");     // no Spotify Connect speaker on the stick (yet)
        s.put("memory_limit_mb", 320);
        // push to talk: the Realtime API, billed by tokens (core/internal/live/realtime.go); Home
        // Assistant's select "Sprachmodell" on the device overrides it (ha.json)
        s.put("realtime_model", "gpt-realtime-mini"); // "text": speech to text and back (core: tvturn.go), slower
        try (FileOutputStream out = new FileOutputStream(new File(files, "settings.json"))) {
            out.write(s.toString(1).getBytes(StandardCharsets.UTF_8));
        }
    }

    private String dnsServers() {
        try {
            ConnectivityManager cm = ctx.getSystemService(ConnectivityManager.class);
            LinkProperties lp = cm.getLinkProperties(cm.getActiveNetwork());
            StringBuilder b = new StringBuilder();
            for (InetAddress a : lp.getDnsServers()) {
                if (b.length() > 0) b.append(',');
                b.append(a.getHostAddress());
            }
            return b.toString();
        } catch (Exception e) {
            return "";
        }
    }
}
