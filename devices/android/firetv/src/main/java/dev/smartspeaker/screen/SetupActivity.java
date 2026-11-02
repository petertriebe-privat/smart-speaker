package dev.smartspeaker.screen;

import android.app.Activity;
import android.content.Intent;
import android.os.Bundle;
import android.util.Log;

// Started over ADB (devices/android/tools/install.sh): stores the extras as the configuration,
// (re)starts the service and closes at once. Without extras it only starts the service.
public class SetupActivity extends Activity {
    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        if (Config.save(this, getIntent().getExtras())) {
            Log.i(ScreenService.TAG, "configuration saved");
            stopService(new Intent(this, ScreenService.class)); // reconnect with the new settings
        }
        startForegroundService(new Intent(this, ScreenService.class));
        finish();
    }
}
