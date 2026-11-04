package dev.jarvis.phone;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.util.Log;

import dev.jarvis.shared.Hud;

// After a restart of the phone or an update of the app the service starts by itself: Jarvis'
// notifications arrive. The microphone (wake word, the car) waits until the app is opened once
// (Android allows no microphone service from the boot).
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context ctx, Intent intent) {
        String action = intent.getAction();
        if (!Intent.ACTION_BOOT_COMPLETED.equals(action) && !Intent.ACTION_MY_PACKAGE_REPLACED.equals(action)) return;
        try {
            ctx.startForegroundService(new Intent(ctx, PhoneService.class));
        } catch (Exception e) {
            Log.w(Hud.TAG, "start at boot: " + e);
        }
    }
}
