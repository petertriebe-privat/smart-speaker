package dev.smartspeaker.screen;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.util.Log;

import dev.smartspeaker.shared.Hud;

// Starts the assistant after a restart of the stick and after an update of the app: through the console
// activity, unseen (ConsoleActivity.BACKGROUND), so the service gets the microphone (Fire OS takes
// the grant itself back on a restart: Home Assistant gives it again, tvkeep.go). The overlay
// permission lets the app open an activity from the background; without it the service starts
// directly (the screen works, the microphone may not).
public class BootReceiver extends BroadcastReceiver {
    @Override
    public void onReceive(Context ctx, Intent intent) {
        Log.i(Hud.TAG, "start after " + intent.getAction());
        try {
            ctx.startActivity(new Intent(ctx, ConsoleActivity.class)
                    .putExtra(ConsoleActivity.BACKGROUND, true)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK | Intent.FLAG_ACTIVITY_NO_ANIMATION));
        } catch (Exception e) {
            Log.w(Hud.TAG, "console not startable: " + e);
            ctx.startForegroundService(new Intent(ctx, ScreenService.class));
        }
    }
}
