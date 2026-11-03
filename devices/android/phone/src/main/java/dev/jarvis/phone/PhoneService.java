package dev.jarvis.phone;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ServiceInfo;
import android.os.Handler;
import android.os.IBinder;
import android.os.Looper;
import android.os.PowerManager;
import android.util.Log;

import androidx.car.app.connection.CarConnection;

import dev.jarvis.phone.car.CarState;
import dev.jarvis.shared.FullscreenActivity;
import dev.jarvis.shared.Hud;

import org.eclipse.paho.client.mqttv3.IMqttActionListener;
import org.eclipse.paho.client.mqttv3.IMqttDeliveryToken;
import org.eclipse.paho.client.mqttv3.IMqttToken;
import org.eclipse.paho.client.mqttv3.MqttAsyncClient;
import org.eclipse.paho.client.mqttv3.MqttCallbackExtended;
import org.eclipse.paho.client.mqttv3.MqttConnectOptions;
import org.eclipse.paho.client.mqttv3.MqttMessage;
import org.eclipse.paho.client.mqttv3.persist.MemoryPersistence;
import org.json.JSONException;
import org.json.JSONObject;

import java.nio.charset.StandardCharsets;

// The phone's Jarvis, kept in a foreground service: the core (PhoneCore) and the phone as one of
// Jarvis' screens, as the Fire TV app's ScreenService. Topics under jarvis/screen/<room id>:
//   status  retained {"state": online|offline, "display": on|off, "room", "name", "version"}; the
//           display is "on" only while the console is open in front, so Jarvis shows cards on the
//           phone only then (cmd/assistant/screenlive.go)
//   show    a card: into the console while it is open, else dropped (no window above other apps)
//   clear   back to the console
//   ack     "<id> fullscreen" once a card is on the screen
public class PhoneService extends Service implements MqttCallbackExtended {
    static final String TAG = Hud.TAG;
    private static final long RETRY_MS = 15000;

    private final Handler main = new Handler(Looper.getMainLooper());
    private Config cfg;
    private MqttAsyncClient mqtt;
    private PhoneCore core;

    @Override
    public void onCreate() {
        super.onCreate();
        NotificationManager nm = getSystemService(NotificationManager.class);
        nm.createNotificationChannel(new NotificationChannel("jarvis", "Jarvis", NotificationManager.IMPORTANCE_MIN));
        nm.createNotificationChannel(new NotificationChannel(Notices.CHANNEL, "Jarvis' Nachrichten",
                NotificationManager.IMPORTANCE_HIGH));
        promote();
        main.post(watchDisplay);
        // In the car (Android Auto) the microphone listens for "Jarvis", the phone's screen off too.
        new CarConnection(this).getType().observeForever(type ->
                PhoneCore.audio(this).setCar(type == CarConnection.CONNECTION_TYPE_PROJECTION));
    }

    // promote: the foreground service, with the microphone when Android allows it. Started at boot
    // (BootReceiver) it may not use the microphone (Android 14): notifications arrive, the wake word
    // waits until the app is opened, which starts the service again from the front and promotes it.
    private void promote() {
        PendingIntent open = PendingIntent.getActivity(this, 0, new Intent(this, PhoneActivity.class),
                PendingIntent.FLAG_IMMUTABLE);
        Notification n = new Notification.Builder(this, "jarvis").setContentTitle("Jarvis")
                .setContentText("Tippen öffnet Jarvis").setContentIntent(open)
                .setSmallIcon(R.drawable.ic_jarvis).setColor(0xFFFF9A1F).build();
        int types = ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK;
        if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
            types |= ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE;
        } else {
            Log.w(TAG, "no microphone permission: Jarvis cannot hear");
        }
        try {
            startForeground(1, n, types);
        } catch (Exception e) {
            Log.w(TAG, "foreground with the microphone refused (started in the background): " + e);
            startForeground(1, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK);
        }
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (core != null) promote(); // started again from the front: the microphone now
        if (core == null) {
            cfg = Config.load(this);
            core = new PhoneCore(this, cfg); // the phone's own Jarvis, if installed with its secrets
            // his state and words come straight from the core, not over the broker
            PhoneCore.audio(this).onCard = json -> main.post(() -> handle("show", json));
            PhoneCore.audio(this).onTool = new PhoneTools(this, PhoneCore.audio(this)); // the phone's tools
            core.start();
            if (cfg.ready()) connect();
            else Log.w(TAG, "no MQTT broker: no cards on the phone (tools/install_phone.sh)");
        }
        return START_STICKY;
    }

    @Override
    public void onDestroy() {
        main.removeCallbacksAndMessages(null);
        if (core != null) core.stop();
        if (mqtt != null) {
            try {
                // Say offline ourselves: a clean disconnect does not fire the last will.
                mqtt.publish(cfg.base() + "/status", status("offline"), 0, true).waitForCompletion(2000);
                mqtt.disconnect().waitForCompletion(2000);
            } catch (Exception e) {
                Log.w(TAG, "disconnect: " + e);
            }
            // Closed in any case: a client left open reconnects with the same id as the next one
            // and the broker throws them out in turn (ScreenService, 29.10.2026).
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

    // display: the console is open in front and the screen is on.
    private boolean display() {
        return FullscreenActivity.consoleOpen() && getSystemService(PowerManager.class).isInteractive();
    }

    private byte[] status(String state) {
        JSONObject s = new JSONObject();
        try {
            s.put("state", state);
            s.put("display", display() ? "on" : "off");
            s.put("room", cfg.room);
            s.put("name", cfg.displayName());
            s.put("media", "idle");
            s.put("kind", "phone"); // takes notifications, its screen on or off (core: screen.Notify)
            s.put("version", BuildConfig.VERSION_NAME);
        } catch (JSONException ignored) {
        }
        return s.toString().getBytes(StandardCharsets.UTF_8);
    }

    // The console opening and closing changes the retained status; looked at every 2 s.
    private boolean lastDisplay;
    private final Runnable watchDisplay = new Runnable() {
        @Override
        public void run() {
            boolean now = display();
            if (now != lastDisplay) {
                lastDisplay = now;
                publishStatus();
            }
            main.postDelayed(this, 2000);
        }
    };

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
        o.setKeepAliveInterval(120);
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

    private String lastState = "";

    // A card goes into the console when it is open in front; the phone draws nothing above other
    // apps, and Jarvis knows it (display off) and speaks instead.
    private void handle(String topic, String payload) {
        if (topic.endsWith("/clear")) {
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
        // the car's screen: his state and words, and every card as a picture
        if ("status".equals(card.optString("kind"))) CarState.status(card);
        else if (!"web".equals(card.optString("mode"))) CarState.show(card);
        // a notification (notify_phone, research_later): on the phone and the car's screen; a
        // tap opens the card. With the console in front the card shows there at once.
        if (card.optBoolean("push") && !FullscreenActivity.consoleOpen()) {
            Notices.post(this, card);
            return;
        }
        if (!FullscreenActivity.consoleOpen() && !FullscreenActivity.open()) return;
        // A new question clears the last card: the weather stayed up while Jarvis spoke about the
        // calendar (01.11.2026). The console comes back, the new card follows if Jarvis shows one.
        if ("status".equals(card.optString("kind"))) {
            String state = card.optString("state");
            boolean newTurn = "listening".equals(state) && !"listening".equals(lastState);
            lastState = state;
            if (newTurn) FullscreenActivity.close();
        }
        Log.i(TAG, "show: " + card.optString("title", card.optString("kind")));
        try {
            // the page and the activity see the chosen mode; a web page stays one
            if (!"web".equals(card.optString("mode"))) card.put("mode", "fullscreen");
        } catch (JSONException ignored) {
        }
        startActivity(new Intent(this, FullscreenActivity.class)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
                .putExtra("card", card.toString()));
        String id = card.optString("id");
        if (!id.isEmpty()) publish("/ack", (id + " fullscreen").getBytes(StandardCharsets.UTF_8), false);
    }
}
