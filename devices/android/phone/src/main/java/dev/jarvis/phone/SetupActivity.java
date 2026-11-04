package dev.jarvis.phone;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.util.Log;

import dev.jarvis.shared.Hud;

// Started over ADB (devices/android/tools/install_phone.sh): stores the extras as the configuration,
// (re)starts the service and closes at once. Being in front, it may start the microphone service
// (Android 14); opening the app again only brought its running console to the front, and the
// service stopped here stayed stopped (01.11.2026).
public class SetupActivity extends Activity {
    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if (Config.save(this, getIntent().getExtras())) {
            Log.i(Hud.TAG, "configuration saved");
            stopService(new Intent(this, PhoneService.class)); // restarted with the new settings
        }
        startForegroundService(new Intent(this, PhoneService.class));
        finish();
    }
}
