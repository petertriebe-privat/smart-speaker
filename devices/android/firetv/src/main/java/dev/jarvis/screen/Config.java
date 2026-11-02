package dev.jarvis.screen;

import android.content.Context;
import android.content.SharedPreferences;
import android.os.Bundle;

// The stick's settings, given once over ADB (SetupActivity) and kept in the app's preferences:
// the MQTT broker and the room this screen stands in. Nothing household-specific is built in.
final class Config {
    private static final String PREFS = "jarvis_screen";
    static final String[] KEYS = {"mqtt_url", "mqtt_user", "mqtt_password", "room", "name"};

    final String mqttUrl, mqttUser, mqttPassword, room, name;

    private Config(SharedPreferences p) {
        mqttUrl = p.getString("mqtt_url", "");
        mqttUser = p.getString("mqtt_user", "");
        mqttPassword = p.getString("mqtt_password", "");
        room = p.getString("room", "");
        name = p.getString("name", "");
    }

    static Config load(Context ctx) {
        return new Config(ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE));
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
        return name.isEmpty() ? "Fire TV " + room : name;
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
