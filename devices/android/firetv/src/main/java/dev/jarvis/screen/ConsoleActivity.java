package dev.jarvis.screen;

import android.Manifest;
import android.app.Activity;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Bundle;

import dev.jarvis.shared.FullscreenActivity;

// The app's icon on the Fire TV home screen opens the Jarvis console: the hologram large in the
// middle, Jarvis' state and the conversation below it, cards on top while he shows something; it
// stays until back. The remote's microphone key talks to him (the stick's own Jarvis, Core.java).
// With BACKGROUND (BootReceiver, the primary's firetv.KeepRunning) it only starts the service and
// closes, unseen: a service started by an activity in front may use the microphone, one started
// by the boot broadcast may not (Android 11).
// Fire OS takes the microphone grant back on every restart (29.10.2026). The service starts at
// once either way; Jarvis has it granted again over Home Assistant's ADB (core: tvkeep.go). Only
// the console, opened by someone in front of the TV, asks for it too: asked unseen at the start,
// the question waited on the screen and Jarvis did not start until it was answered.
public class ConsoleActivity extends Activity {
    static final String CONSOLE = "{\"mode\":\"fullscreen\",\"kind\":\"status\",\"state\":\"idle\",\"console\":true}";
    static final String BACKGROUND = "background";
    private static final int ASK_MICROPHONE = 1;

    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        startForegroundService(new Intent(this, ScreenService.class));
        if (getIntent().getBooleanExtra(BACKGROUND, false)) {
            finish();
            return;
        }
        if (checkSelfPermission(Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED) {
            openConsole();
        } else {
            requestPermissions(new String[]{Manifest.permission.RECORD_AUDIO}, ASK_MICROPHONE);
        }
    }

    @Override
    public void onRequestPermissionsResult(int code, String[] permissions, int[] results) {
        openConsole(); // granted or not: the screen works either way, the microphone retries by itself
    }

    private void openConsole() {
        startActivity(new Intent(this, FullscreenActivity.class).putExtra("card", CONSOLE));
        finish();
    }
}
