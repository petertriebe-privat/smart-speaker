package dev.smartspeaker.phone;

import android.content.Context;
import android.net.ConnectivityManager;
import android.net.LinkProperties;
import android.net.Network;
import android.provider.Settings;
import android.util.Log;

import dev.smartspeaker.shared.AudioBridge;
import dev.smartspeaker.shared.CoreProcess;
import dev.smartspeaker.shared.Hud;

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

// The phone's own assistant: the core (core/, board phone) runs next to the HUD, the phone's
// microphone and speaker through the AudioBridge, the assistant's button over its "keys" connection. Never
// the household's primary (no pollers, no scheduler there: they stay on the leader Echo). On only
// when the install script left the household's secrets in the app's files.
public final class PhoneCore {
    private static AudioBridge bridge;

    // audio: the one bridge of the app, also the HUD's button (FullscreenActivity.talk).
    public static synchronized AudioBridge audio(Context ctx) {
        if (bridge == null) bridge = AudioBridge.forPhone(ctx.getApplicationContext());
        return bridge;
    }

    private final Context ctx;
    private final Config cfg;
    private CoreProcess process;
    private boolean audioStarted;

    PhoneCore(Context ctx, Config cfg) {
        this.ctx = ctx;
        this.cfg = cfg;
    }

    boolean configured() {
        return new File(ctx.getFilesDir(), "secrets.json").exists() && CoreProcess.binary(ctx).exists();
    }

    void start() {
        if (!configured()) {
            Log.i(Hud.TAG, "core off (no secrets.json: tools/install_phone.sh)");
            return;
        }
        File files = ctx.getFilesDir();
        try {
            writeSettings(files);
            if (!audioStarted) audio(ctx).start();
            audioStarted = true;
        } catch (Exception e) {
            Log.e(Hud.TAG, "core: " + e);
            return;
        }
        List<String> args = new ArrayList<>(Arrays.asList(CoreProcess.binary(ctx).getPath(),
                "-secrets", new File(files, "secrets.json").getPath(),
                "-settings", new File(files, "settings.json").getPath(),
                "-ctl", new File(files, "run/ctl.sock").getPath()));
        Map<String, String> env = new HashMap<>();
        env.put("ASSISTANT_BOARD", "phone");
        env.put("ASSISTANT_AUDIO", "127.0.0.1:" + AudioBridge.PORT);
        env.put("ASSISTANT_ID", Settings.Secure.getString(ctx.getContentResolver(), Settings.Secure.ANDROID_ID));
        env.put("ASSISTANT_PACKAGE", ctx.getPackageName());
        env.put("ASSISTANT_DNS", dnsServers());
        // the current network's servers at every change (Wi-Fi, mobile data): read at each lookup
        File dns = new File(files, "run/dns.txt");
        env.put("ASSISTANT_DNS_FILE", dns.getPath());
        AudioBridge.aecDump = new File(files, "run/aec-dump"); // tuning the echo canceller (AudioBridge.dump)
        watchNetwork(dns);
        env.put("HOME", files.getPath());
        env.put("TMPDIR", ctx.getCacheDir().getPath());
        process = new CoreProcess(ctx, args, env);
        process.start();
    }

    private ConnectivityManager.NetworkCallback network;

    // watchNetwork keeps dns.txt at the default network's DNS servers, public ones after them. The
    // home router stayed first on mobile data and nothing resolved (01.11.2026).
    private void watchNetwork(File dns) {
        ConnectivityManager cm = ctx.getSystemService(ConnectivityManager.class);
        if (network != null) cm.unregisterNetworkCallback(network);
        network = new ConnectivityManager.NetworkCallback() {
            @Override
            public void onLinkPropertiesChanged(Network n, LinkProperties lp) {
                StringBuilder b = new StringBuilder();
                for (InetAddress a : lp.getDnsServers()) b.append(a.getHostAddress()).append(',');
                b.append("1.1.1.1,8.8.8.8");
                try {
                    File tmp = new File(dns.getPath() + ".new");
                    try (FileOutputStream out = new FileOutputStream(tmp)) {
                        out.write(b.toString().getBytes(StandardCharsets.UTF_8));
                    }
                    if (!tmp.renameTo(dns)) Log.w(Hud.TAG, "dns.txt not replaced");
                    Log.i(Hud.TAG, "network changed: DNS " + b);
                } catch (Exception e) {
                    Log.w(Hud.TAG, "dns.txt: " + e);
                }
            }
        };
        cm.registerDefaultNetworkCallback(network);
    }

    void stop() {
        if (network != null) {
            ctx.getSystemService(ConnectivityManager.class).unregisterNetworkCallback(network);
            network = null;
        }
        if (process != null) process.stop();
        if (audioStarted) audio(ctx).stop();
        audioStarted = false;
    }

    // settings.json for the core, rewritten at every start; device.json next to it (from the
    // install script) may override any of it, as on the Echos.
    private void writeSettings(File files) throws Exception {
        new File(files, "state").mkdirs();
        new File(files, "run").mkdirs();
        JSONObject s = new JSONObject();
        s.put("state_dir", new File(files, "state").getPath());
        s.put("run_dir", new File(files, "run").getPath());
        s.put("room", cfg.room);       // the name of the phone's screen; the phone stands in no room
        s.put("name", cfg.displayName());
        s.put("primary", false);       // never the household's leader (#75) …
        s.put("no_lead", true);        // … not even when the leader is silent
        s.put("volume", 100);          // the phone's own volume rules
        // the wake word while the console is open in front (the app opens the microphone only then):
        // the household's model if the install script brought it, else the built-in "Hey Jarvis"
        s.put("wake_word", true);
        s.put("wake_model", "wakemodel/assistant.tflite");
        s.put("wake_cutoff", 0.85);    // as the Echos
        s.put("echo_gate", false);
        s.put("music_ring", false);
        s.put("spotify_name", "");     // no Spotify Connect speaker on the phone (yet)
        s.put("memory_limit_mb", 400);
        s.put("realtime_model", "");   // GPT Live: a tapped conversation needs its turn detection
        try (FileOutputStream out = new FileOutputStream(new File(files, "settings.json"))) {
            out.write(s.toString(1).getBytes(StandardCharsets.UTF_8));
        }
    }

    // The network's DNS servers at the start, public ones after them: the phone changes between
    // Wi-Fi and mobile data, and the core keeps what it got.
    private String dnsServers() {
        StringBuilder b = new StringBuilder();
        try {
            ConnectivityManager cm = ctx.getSystemService(ConnectivityManager.class);
            LinkProperties lp = cm.getLinkProperties(cm.getActiveNetwork());
            for (InetAddress a : lp.getDnsServers()) {
                if (b.length() > 0) b.append(',');
                b.append(a.getHostAddress());
            }
        } catch (Exception ignored) {
        }
        if (b.length() > 0) b.append(',');
        return b.append("1.1.1.1,8.8.8.8").toString();
    }
}
