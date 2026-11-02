package dev.smartspeaker.phone;

import android.app.PendingIntent;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;

import androidx.car.app.notification.CarAppExtender;
import androidx.car.app.notification.CarPendingIntent;
import androidx.core.app.NotificationCompat;
import androidx.core.app.NotificationManagerCompat;

import dev.smartspeaker.phone.car.AssistantCarService;

import org.json.JSONObject;

import java.util.concurrent.atomic.AtomicInteger;

// The assistant's notifications on the phone (#75: notify_phone, research_later): the card's title and text,
// shown on the car's screen too while Android Auto runs (CarAppExtender). A tap on the phone opens
// the card in the assistant's console; on the car it opens the assistant there.
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
                        .setData(android.net.Uri.parse("assistant://card/" + System.currentTimeMillis()))
                        .putExtra(PhoneActivity.CARD, card.toString())
                        .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
                PendingIntent.FLAG_IMMUTABLE | PendingIntent.FLAG_UPDATE_CURRENT);
        PendingIntent car = CarPendingIntent.getCarApp(ctx, id,
                new Intent(Intent.ACTION_VIEW).setComponent(new ComponentName(ctx, AssistantCarService.class)),
                PendingIntent.FLAG_IMMUTABLE);
        NotificationCompat.Builder b = new NotificationCompat.Builder(ctx, CHANNEL)
                .setSmallIcon(android.R.drawable.ic_btn_speak_now)
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
                        .setSmallIcon(android.R.drawable.ic_btn_speak_now)
                        .setImportance(android.app.NotificationManager.IMPORTANCE_HIGH)
                        .setContentIntent(car)
                        .build());
        try {
            NotificationManagerCompat.from(ctx).notify(id, b.build());
        } catch (SecurityException e) {
            android.util.Log.w(dev.smartspeaker.shared.Hud.TAG, "notification: " + e);
        }
    }
}
