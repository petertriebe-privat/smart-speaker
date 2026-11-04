package dev.jarvis.screen;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.Service;
import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.IntentFilter;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.media.AudioManager;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.os.PowerManager;
import android.provider.Settings;
import android.util.Log;
import android.view.KeyEvent;

import org.eclipse.paho.client.mqttv3.IMqttActionListener;
import org.eclipse.paho.client.mqttv3.IMqttDeliveryToken;
import org.eclipse.paho.client.mqttv3.IMqttToken;
import org.eclipse.paho.client.mqttv3.MqttAsyncClient;
import org.eclipse.paho.client.mqttv3.MqttCallbackExtended;
import org.eclipse.paho.client.mqttv3.MqttConnectOptions;
import org.eclipse.paho.client.mqttv3.MqttMessage;
import org.eclipse.paho.client.mqttv3.persist.MemoryPersistence;
import dev.jarvis.shared.FullscreenActivity;
import dev.jarvis.shared.Hud;
import dev.jarvis.shared.Overlay;

import org.json.JSONArray;
import org.json.JSONException;
import org.json.JSONObject;

import java.nio.charset.StandardCharsets;

// The screen's link to Jarvis: one MQTT connection, kept in a foreground service so Fire OS
// leaves it running. Topics under jarvis/screen/<room id>:
//   status  retained JSON {"state": online|offline, "display": on|off, "room", "name", "version"};
//           the broker sets offline (last will) when the stick drops off
//   show    a card (JSON, see assets/jarvis.js): mode overlay (above any app), fullscreen, or
//           web (a web page over the whole screen: {"mode": "web", "url"})
//   clear   hides what is shown
//   ack     the id of a card once it is on the screen
public class ScreenService extends Service implements MqttCallbackExtended {
    static final String TAG = Hud.TAG;
    private static final long RETRY_MS = 15000;

    private final Handler main = new Handler(Looper.getMainLooper());
    private Config cfg;
    private MqttAsyncClient mqtt;
    private Overlay overlay;
    private Core core;
    private BroadcastReceiver display;

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel("screen", "Jarvis", NotificationManager.IMPORTANCE_MIN));
        Notification n = new Notification.Builder(this, "screen").setContentTitle("Jarvis")
                .setSmallIcon(android.R.drawable.ic_dialog_info).build();
        if (Build.VERSION.SDK_INT >= 30) {
            // the microphone type lets the core hear the remote while another app is in front
            startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE
                    | ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK);
        } else if (Build.VERSION.SDK_INT >= 29) {
            startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK);
        } else {
            startForeground(1, n);
        }
        overlay = new Overlay(this);
        display = new BroadcastReceiver() {
            @Override
            public void onReceive(Context ctx, Intent intent) {
                publishStatus();
            }
        };
        IntentFilter filter = new IntentFilter(Intent.ACTION_SCREEN_ON);
        filter.addAction(Intent.ACTION_SCREEN_OFF);
        registerReceiver(display, filter);
        main.post(watchMedia);
        Hud.listener = ScreenService::cardClosed;
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (mqtt == null) {
            cfg = Config.load(this);
            if (!cfg.ready()) {
                Log.w(TAG, "not configured (mqtt_url, room): see devices/android/tools/install.sh");
                stopSelf();
                return START_NOT_STICKY;
            }
            Log.i(TAG, "room " + cfg.room + ", overlay allowed " + Settings.canDrawOverlays(this));
            connect();
            core = new Core(this, cfg); // the stick's own Jarvis, if installed with --core
            core.start();
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        unregisterReceiver(display);
        if (core != null) core.stop();
        overlay.hide();
        if (mqtt != null) {
            try {
                // Say offline ourselves: a clean disconnect does not fire the last will.
                mqtt.publish(cfg.base() + "/status", status("offline"), 0, true).waitForCompletion(2000);
                mqtt.disconnect().waitForCompletion(2000);
            } catch (Exception e) {
                Log.w(TAG, "disconnect: " + e);
            }
            // Closed in any case: a client left open went on reconnecting with the same id as the
            // next one, the broker threw them out in turn every second and no card arrived
            // (29.10.2026, the bedroom TV).
            try {
                mqtt.disconnectForcibly(0, 1000);
            } catch (Exception ignored) {
            }
            try {
                mqtt.close(true);
            } catch (Exception ignored) {
            }
            mqtt = null;
        }
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }

    private byte[] status(String state) {
        JSONObject s = new JSONObject();
        try {
            PowerManager pm = getSystemService(PowerManager.class);
            s.put("state", state);
            s.put("display", pm.isInteractive() ? "on" : "off");
            s.put("room", cfg.room);
            s.put("name", cfg.displayName());
            s.put("media", playing() ? "playing" : "idle");
            s.put("version", BuildConfig.VERSION_NAME);
        } catch (JSONException ignored) {
        }
        return s.toString().getBytes(StandardCharsets.UTF_8);
    }

    private void connect() {
        try {
            String id = "jarvis-screen-" + Config.topicId(cfg.room);
            mqtt = new MqttAsyncClient(cfg.brokerUri(), id, new MemoryPersistence());
            mqtt.setCallback(this);
        } catch (Exception e) {
            Log.e(TAG, "MQTT client: " + e);
            return;
        }
        tryConnect();
    }

    // The first connect is retried here; after one success Paho reconnects by itself.
    private void tryConnect() {
        MqttConnectOptions o = new MqttConnectOptions();
        o.setCleanSession(true);
        o.setAutomaticReconnect(true);
        o.setKeepAliveInterval(60);
        if (!cfg.mqttUser.isEmpty()) {
            o.setUserName(cfg.mqttUser);
            o.setPassword(cfg.mqttPassword.toCharArray());
        }
        o.setWill(cfg.base() + "/status", status("offline"), 0, true);
        try {
            mqtt.connect(o, null, new IMqttActionListener() {
                @Override
                public void onSuccess(IMqttToken token) {
                }

                @Override
                public void onFailure(IMqttToken token, Throwable e) {
                    Log.w(TAG, "connect failed: " + e + ", again in " + RETRY_MS / 1000 + " s");
                    main.postDelayed(() -> { if (mqtt != null) tryConnect(); }, RETRY_MS);
                }
            });
        } catch (Exception e) {
            Log.e(TAG, "connect: " + e);
        }
    }

    private void publishStatus() {
        publish("/status", status("online"), true);
    }

    private void publish(String suffix, byte[] payload, boolean retained) {
        MqttAsyncClient c = mqtt;
        if (c == null || !c.isConnected()) return;
        try {
            c.publish(cfg.base() + suffix, payload, 0, retained);
        } catch (Exception e) {
            Log.w(TAG, "publish " + suffix + ": " + e);
        }
    }

    @Override
    public void connectComplete(boolean reconnect, String uri) {
        Log.i(TAG, (reconnect ? "reconnected to " : "connected to ") + uri);
        try {
            mqtt.subscribe(new String[]{cfg.base() + "/show", cfg.base() + "/clear"}, new int[]{0, 0});
        } catch (Exception e) {
            Log.e(TAG, "subscribe: " + e);
        }
        publishStatus();
    }

    @Override
    public void connectionLost(Throwable cause) {
        Log.w(TAG, "connection lost: " + cause);
    }

    @Override
    public void messageArrived(String topic, MqttMessage message) {
        String payload = new String(message.getPayload(), StandardCharsets.UTF_8);
        main.post(() -> handle(topic, payload));
    }

    @Override
    public void deliveryComplete(IMqttDeliveryToken token) {
    }

    // playing: some app plays sound (a film, music) – the cheap sign that the TV is in use.
    private boolean playing() {
        return getSystemService(AudioManager.class).isMusicActive();
    }

    // The media state changes the retained status (Jarvis decides by it too); looked at every 5 s.
    private boolean lastPlaying;
    private final Runnable watchMedia = new Runnable() {
        @Override
        public void run() {
            boolean now = playing();
            if (now != lastPlaying) {
                lastPlaying = now;
                publishStatus();
            }
            main.postDelayed(this, 5000);
        }
    };

    // A film this app paused for a full-screen card; it plays on when the card closes.
    private static volatile boolean pausedFilm;

    private static void mediaKey(Context ctx, int code) {
        AudioManager am = ctx.getSystemService(AudioManager.class);
        am.dispatchMediaKeyEvent(new KeyEvent(KeyEvent.ACTION_DOWN, code));
        am.dispatchMediaKeyEvent(new KeyEvent(KeyEvent.ACTION_UP, code));
    }

    // cardClosed: the full screen closed (FullscreenActivity): a film paused for it plays on.
    static void cardClosed(Context ctx) {
        if (pausedFilm) {
            pausedFilm = false;
            mediaKey(ctx, KeyEvent.KEYCODE_MEDIA_PLAY);
            Log.i(TAG, "film resumed");
        }
    }

    // chooseMode for mode "auto" (Jarvis may also name overlay or fullscreen): hints stay small,
    // alarms, web pages and videos take the screen; otherwise the whole screen when nothing plays,
    // and while a film plays only what is too much for the corner.
    static String chooseMode(JSONObject card, boolean playing) {
        String kind = card.optString("kind");
        if (card.optBoolean("notify") || kind.equals("message") || kind.equals("call") || kind.equals("camera")
                || kind.equals("package") || kind.equals("status")) return "overlay";
        if (kind.equals("alert") || kind.equals("web") || kind.equals("video") || card.has("url")) return "fullscreen";
        if (!playing) return "fullscreen";
        return large(card) ? "fullscreen" : "overlay";
    }

    // large: more than a glance in the corner can carry.
    static boolean large(JSONObject card) {
        String kind = card.optString("kind");
        if (kind.equals("recipe") || kind.equals("search") || kind.equals("choice") || kind.equals("calendar")) return true;
        JSONArray items = card.optJSONArray("items");
        if (items != null && items.length() > 8) return true;
        JSONArray blocks = card.optJSONArray("blocks");
        if (blocks == null) return false;
        if (blocks.length() > 2) return true;
        for (int i = 0; i < blocks.length(); i++) {
            JSONObject b = blocks.optJSONObject(i);
            if (b == null) continue;
            switch (b.optString("type")) {
                // pictures too: a gallery ended up in the now small corner above a film (29.10.2026)
                case "steps": case "table": case "chart": case "timeline": case "choices": case "video": case "web":
                case "gallery": case "image":
                    return true;
            }
            JSONArray list = b.optJSONArray("items");
            if (list != null && list.length() > 8) return true;
        }
        return false;
    }

    // wake ends Fire TV's screensaver (it lies above every app and overlay) and turns a dozing
    // screen on: a wake lock that causes a wakeup, held only a moment.
    @SuppressWarnings("deprecation")
    private void wake() {
        PowerManager pm = getSystemService(PowerManager.class);
        PowerManager.WakeLock lock = pm.newWakeLock(PowerManager.SCREEN_BRIGHT_WAKE_LOCK
                | PowerManager.ACQUIRE_CAUSES_WAKEUP | PowerManager.ON_AFTER_RELEASE, "jarvis:show");
        lock.acquire(1000);
    }

    private void handle(String topic, String payload) {
        if (topic.endsWith("/clear")) {
            overlay.hide();
            FullscreenActivity.close();
            return;
        }
        JSONObject card;
        try {
            card = new JSONObject(payload);
        } catch (JSONException e) {
            Log.w(TAG, "bad card: " + e);
            return;
        }
        boolean filmPlays = playing();
        String mode = card.optString("mode", "auto");
        if ("status".equals(card.optString("kind"))) {
            // Jarvis' state and words: into the console when it is open; a conversation held
            // through this TV ("here") in the corner; another device's conversation in this room
            // in the corner only while no film plays.
            if (FullscreenActivity.consoleOpen() || FullscreenActivity.open()) mode = "fullscreen"; // the avatar there
            else if (filmPlays && !card.optBoolean("here")) mode = "none";
            else mode = "overlay";
        }
        if ("auto".equals(mode)) mode = chooseMode(card, filmPlays);
        if ("none".equals(mode)) return;
        Log.i(TAG, "show " + mode + (filmPlays ? " (media playing)" : "") + ": " + card.optString("title"));
        wake();
        if ("fullscreen".equals(mode) || "web".equals(mode)) {
            overlay.hide();
            if (filmPlays && !card.optBoolean("keep_playing") && !"status".equals(card.optString("kind"))) {
                mediaKey(this, KeyEvent.KEYCODE_MEDIA_PAUSE);
                pausedFilm = true;
                Log.i(TAG, "film paused for the card");
            }
            try {
                card.put("mode", mode); // the page and the activity see the chosen mode
            } catch (JSONException ignored) {
            }
            Intent i = new Intent(this, FullscreenActivity.class)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                    .putExtra("card", card.toString());
            startActivity(i); // allowed from the background: the app may draw over others
        } else {
            if (!FullscreenActivity.consoleOpen()) FullscreenActivity.close();
            overlay.show(card);
        }
        // ack: "<id> <mode>[ paused]", so Jarvis can say where the card went
        String id = card.optString("id");
        String ack = id + " " + mode + (pausedFilm && !"overlay".equals(mode) ? " paused" : "");
        if (!id.isEmpty()) publish("/ack", ack.getBytes(StandardCharsets.UTF_8), false);
    }
}
