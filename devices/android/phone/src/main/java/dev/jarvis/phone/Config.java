package dev.jarvis.phone;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Bundle;
import android.util.Log;

import dev.jarvis.shared.Hud;

import org.json.JSONObject;

import java.io.File;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;

// The phone's settings: the name of its screen (the "room" its cards go to; the phone stands in
// none) and Jarvis' name, given once over ADB (SetupActivity) and kept in the app's preferences;
// the MQTT broker from the household's secrets in the app's files (install_phone.sh puts them
// there; given as ADB extras they landed in the phone's system log). Nothing household-specific
// is built in.
final class Config {
    private static final String PREFS = "jarvis_phone";
    static final String[] KEYS = {"room", "name"};

    final String mqttUrl, mqttUser, mqttPassword, room, name;

    // The core's home tunnel (core: cmd/assistant/phoneboard.go, tunnelMQTT): with the way home set
    // up the app's broker connection goes through it too, at home and away.
    private static final String TUNNEL_MQTT = "mqtt://127.0.0.1:41883";

    private Config(SharedPreferences p, JSONObject secrets) {
        mqttUrl = secrets.optString("HOME_ENDPOINTS").isEmpty() ? secrets.optString("MQTT_URL") : TUNNEL_MQTT;
        mqttUser = secrets.optString("MQTT_USER");
        mqttPassword = secrets.optString("MQTT_PASSWORD");
        room = p.getString("room", "Handy");
        name = p.getString("name", "");
    }

    static Config load(Context ctx) {
        JSONObject secrets = new JSONObject();
        try {
            byte[] data = Files.readAllBytes(new File(ctx.getFilesDir(), "secrets.json").toPath());
            secrets = new JSONObject(new String(data, StandardCharsets.UTF_8));
        } catch (Exception e) {
            Log.w(Hud.TAG, "no secrets.json: " + e);
        }
        return new Config(ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE), secrets);
    }

    // save keeps every known key present in extras; true if anything changed.
    static boolean save(Context ctx, Bundle extras) {
        if (extras == null) return false;
        SharedPreferences.Editor e = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit();
        boolean changed = false;
        for (String key : KEYS) {
            if (extras.containsKey(key)) {
                e.putString(key, extras.getString(key, ""));
                changed = true;
            }
        }
        e.apply();
        return changed;
    }

    boolean ready() {
        return !mqttUrl.isEmpty() && !room.isEmpty();
    }

    // Paho wants tcp://; the Echo's secrets say mqtt://.
    String brokerUri() {
        String uri = mqttUrl.replaceFirst("^mqtt://", "tcp://");
        return uri.matches(".*:\\d+$") ? uri : uri + ":1883";
    }

    String displayName() {
        return name.isEmpty() ? "Jarvis " + room : name;
    }

    // The topic base of this screen: jarvis/screen/<room id>.
    String base() {
        return "jarvis/screen/" + topicId(room);
    }

    // topicId as mqtt.TopicID on the Echo: lower case, [a-z0-9_], umlauts spelled out.
    static String topicId(String name) {
        StringBuilder b = new StringBuilder();
        for (char c : name.toLowerCase().toCharArray()) {
            if (c >= 'a' && c <= 'z' || c >= '0' && c <= '9') b.append(c);
            else if (c == 'ä') b.append("ae");
            else if (c == 'ö') b.append("oe");
            else if (c == 'ü') b.append("ue");
            else if (c == 'ß') b.append("ss");
            else if (b.length() > 0 && b.charAt(b.length() - 1) != '_') b.append('_');
        }
        int end = b.length();
        while (end > 0 && b.charAt(end - 1) == '_') end--;
        return b.substring(0, end);
    }
}
