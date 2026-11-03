package dev.jarvis.phone;

import android.app.PendingIntent;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;

import androidx.car.app.notification.CarAppExtender;
import androidx.car.app.notification.CarPendingIntent;
import androidx.core.app.NotificationCompat;
import androidx.core.app.NotificationManagerCompat;

import dev.jarvis.phone.car.JarvisCarService;

import org.json.JSONObject;

import java.util.concurrent.atomic.AtomicInteger;

// Jarvis' notifications on the phone (#75: notify_phone, research_later): the card's title and text,
// shown on the car's screen too while Android Auto runs (CarAppExtender). A tap on the phone opens
// the card in Jarvis' console; on the car it opens Jarvis there.
final class Notices {
    static final String CHANNEL = "messages";
    private static final AtomicInteger next = new AtomicInteger(100);

    private Notices() {
    }

    static void post(Context ctx, JSONObject card) {
        int id = next.incrementAndGet();
        String title = card.optString("title", "Jarvis"), text = card.optString("text", "");
        // its own action and data: with the app's plain launch intent Android only brought the
        // running console to the front, and the card never arrived (01.11.2026)
        PendingIntent open = PendingIntent.getActivity(ctx, id,
                new Intent(ctx, PhoneActivity.class).setAction(PhoneActivity.SHOW_CARD)
                        .setData(android.net.Uri.parse("jarvis://card/" + System.currentTimeMillis()))
                        .putExtra(PhoneActivity.CARD, card.toString())
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        PendingIntent car = CarPendingIntent.getCarApp(ctx, id,
                new Intent(Intent.ACTION_VIEW).setComponent(new ComponentName(ctx, JarvisCarService.class)),
                PendingIntent.FLAG_IMMUTABLE);
        // Jarvis' hologram as the large icon (the phone and the car's banner), his orb in one
        // colour as the small one (a microphone stood there before)
        android.graphics.Bitmap face = android.graphics.BitmapFactory.decodeResource(ctx.getResources(), R.drawable.icon);
        NotificationCompat.Builder b = new NotificationCompat.Builder(ctx, CHANNEL)
                .setSmallIcon(R.drawable.ic_jarvis)
                .setLargeIcon(face)
                .setColor(0xFFFF9A1F) // the HUD's amber
                .setContentTitle(title)
                .setContentText(text)
                .setStyle(new NotificationCompat.BigTextStyle().bigText(text))
                .setPriority(NotificationCompat.PRIORITY_HIGH)
                .setCategory(NotificationCompat.CATEGORY_REMINDER)
                .setAutoCancel(true)
                .setContentIntent(open)
                .extend(new CarAppExtender.Builder()
                        .setContentTitle(title)
                        .setContentText(text.length() > 200 ? text.substring(0, 200) + " …" : text)
                        .setSmallIcon(R.drawable.ic_jarvis)
                        .setLargeIcon(face)
                        .setImportance(android.app.NotificationManager.IMPORTANCE_HIGH)
                        .setContentIntent(car)
                        .build());
        try {
            NotificationManagerCompat.from(ctx).notify(id, b.build());
        } catch (SecurityException e) {
            android.util.Log.w(dev.jarvis.shared.Hud.TAG, "notification: " + e);
        }
    }
}
