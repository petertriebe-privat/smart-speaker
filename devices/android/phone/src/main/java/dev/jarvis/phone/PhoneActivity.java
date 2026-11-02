package dev.jarvis.phone;

import android.Manifest;
import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Build;
import android.os.Bundle;

import dev.jarvis.shared.FullscreenActivity;

import java.util.ArrayList;
import java.util.List;

// The app's icon opens the Jarvis console as on the Fire TV: the hologram on top, his state and
// the conversation below, his cards in between, Jarvis' button at the bottom. First it asks for
// the microphone (and to show its notification), then starts the service: a microphone service
// may start only while the app is in front (Android 14).
public class PhoneActivity extends Activity {
    static final String CONSOLE = "{\"mode\":\"fullscreen\",\"kind\":\"status\",\"state\":\"idle\",\"console\":true}";
    private static final int ASK = 1;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        List<String> ask = new ArrayList<>();
        if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) != PackageManager.PERMISSION_GRANTED) {
            ask.add(Manifest.permission.RECORD_AUDIO);
        }
        if (Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            ask.add(Manifest.permission.POST_NOTIFICATIONS);
        }
        if (ask.isEmpty()) open();
        else requestPermissions(ask.toArray(new String[0]), ASK);
    }

    @Override
    public void onRequestPermissionsResult(int code, String[] permissions, int[] results) {
        open(); // granted or not: the screen works either way, the service says what is missing
    }

    // CARD: a notification's card (Notices), shown over the console once it is open.
    static final String CARD = "card";
    static final String SHOW_CARD = "dev.jarvis.phone.SHOW_CARD";

    private void open() {
        startForegroundService(new Intent(this, PhoneService.class));
        FullscreenActivity.talk = PhoneCore.audio(this);
        startActivity(new Intent(this, FullscreenActivity.class).putExtra("card", CONSOLE));
        String card = getIntent().getStringExtra(CARD);
        if (card != null) {
            try {
                org.json.JSONObject c = new org.json.JSONObject(card);
                c.put("mode", "fullscreen");
                c.remove("push");
                startActivity(new Intent(this, FullscreenActivity.class).putExtra("card", c.toString()));
            } catch (Exception ignored) {
            }
        }
        finish();
    }
}
