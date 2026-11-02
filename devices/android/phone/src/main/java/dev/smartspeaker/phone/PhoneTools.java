package dev.smartspeaker.phone;

import android.Manifest;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.database.Cursor;
import android.media.AudioManager;
import android.net.Uri;
import android.os.Handler;
import android.os.Looper;
import android.provider.AlarmClock;
import android.provider.ContactsContract;
import android.telephony.SmsManager;
import android.util.Log;
import android.view.KeyEvent;

import dev.smartspeaker.phone.car.AssistantCarService;
import dev.smartspeaker.shared.AudioBridge;
import dev.smartspeaker.shared.Hud;

import org.json.JSONObject;

import java.util.List;
import java.util.Locale;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

// The phone's tools (core: cmd/assistant/phonetools.go): navigation, calls, messages, SMS, media,
// apps, alarms. Each answers with a text for the model. Activities start from the background only
// because the app may draw over others (install_phone.sh grants it), as Android's exemption.
final class PhoneTools implements AudioBridge.Tools {
    private final Context ctx;
    private final AudioBridge bridge;
    private final ExecutorService work = Executors.newSingleThreadExecutor();
    private final Handler main = new Handler(Looper.getMainLooper());

    PhoneTools(Context ctx, AudioBridge bridge) {
        this.ctx = ctx.getApplicationContext();
        this.bridge = bridge;
    }

    @Override
    public void run(String id, String json) {
        work.execute(() -> {
            String out;
            try {
                JSONObject call = new JSONObject(json);
                JSONObject args = call.optJSONObject("args");
                if (args == null) args = new JSONObject();
                out = dispatch(call.optString("name"), args);
            } catch (Exception e) {
                out = "Error: " + e;
            }
            Log.i(Hud.TAG, "tool: " + json.substring(0, Math.min(120, json.length())) + " -> " + out.substring(0, Math.min(120, out.length())));
            bridge.result(id, out);
        });
    }

    private String dispatch(String name, JSONObject a) {
        switch (name) {
            case "phone_navigate":
                return navigate(a.optString("destination"), a.optString("mode", "driving"));
            case "phone_call":
                return call(a.optString("who"));
            case "phone_messages":
                return messages(a.optString("app"));
            case "phone_reply":
                return reply(a.optString("key"), a.optString("sender"), a.optString("text"));
            case "phone_sms":
                return sms(a.optString("to"), a.optString("text"));
            case "phone_media":
                return media(a.optString("action"));
            case "phone_open_app":
                return openApp(a.optString("app"));
            case "phone_alarm":
                return alarm(a.optString("time"), a.optInt("seconds"), a.optString("label"));
        }
        return "Error: unknown tool " + name;
    }

    private boolean granted(String permission) {
        return ctx.checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED;
    }

    private String start(Intent i) {
        i.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
        try {
            ctx.startActivity(i);
            return "";
        } catch (Exception e) {
            return "Error: " + e.getMessage();
        }
    }

    // --- navigation: on the car's screen while Android Auto runs the assistant, else Google Maps ---
    private String navigate(String destination, String mode) {
        if (destination.isEmpty()) return "Error: where to?";
        if (AssistantCarService.navigate(destination)) return "Navigation to " + destination + " started on the car's screen";
        String m = mode.startsWith("walk") ? "w" : mode.startsWith("bic") ? "b" : mode.startsWith("trans") ? "r" : "d";
        Intent i = new Intent(Intent.ACTION_VIEW, Uri.parse("google.navigation:q=" + Uri.encode(destination) + "&mode=" + m));
        i.setPackage("com.google.android.apps.maps");
        String err = start(i);
        if (!err.isEmpty()) {
            err = start(new Intent(Intent.ACTION_VIEW, Uri.parse("geo:0,0?q=" + Uri.encode(destination))));
        }
        return err.isEmpty() ? "Navigation to " + destination + " started in Google Maps" : err;
    }

    // --- contacts: a name to a number from the phone's contacts ---
    static final class Contact {
        String name, number;
    }

    private Contact contact(String who) {
        Contact c = new Contact();
        String digits = who.replaceAll("[^0-9+]", "");
        if (digits.length() >= 5 && digits.length() * 2 >= who.replace(" ", "").length()) {
            c.name = who;
            c.number = digits;
            return c;
        }
        if (!granted(Manifest.permission.READ_CONTACTS)) return null;
        Uri uri = Uri.withAppendedPath(ContactsContract.CommonDataKinds.Phone.CONTENT_FILTER_URI, Uri.encode(who));
        try (Cursor cur = ctx.getContentResolver().query(uri, new String[]{
                ContactsContract.CommonDataKinds.Phone.DISPLAY_NAME, ContactsContract.CommonDataKinds.Phone.NUMBER,
                ContactsContract.CommonDataKinds.Phone.TYPE}, null, null, null)) {
            if (cur == null) return null;
            Contact first = null;
            while (cur.moveToNext()) {
                Contact x = new Contact();
                x.name = cur.getString(0);
                x.number = cur.getString(1);
                if (first == null) first = x;
                if (cur.getInt(2) == ContactsContract.CommonDataKinds.Phone.TYPE_MOBILE) return x;
            }
            return first;
        }
    }

    private String call(String who) {
        if (!granted(Manifest.permission.CALL_PHONE)) return "Error: The assistant may not make calls on this phone";
        Contact c = contact(who);
        if (c == null) return "Error: no contact \"" + who + "\" on the phone";
        String err = start(new Intent(Intent.ACTION_CALL, Uri.parse("tel:" + Uri.encode(c.number))));
        return err.isEmpty() ? "Calling " + c.name + " (" + c.number + ")" : err;
    }

    private String sms(String to, String text) {
        if (!granted(Manifest.permission.SEND_SMS)) return "Error: The assistant may not send SMS on this phone";
        if (text.isEmpty()) return "Error: the text is missing";
        Contact c = contact(to);
        if (c == null) return "Error: no contact \"" + to + "\" on the phone";
        SmsManager sms = ctx.getSystemService(SmsManager.class);
        sms.sendMultipartTextMessage(c.number, null, sms.divideMessage(text), null, null);
        return "SMS sent to " + c.name + " (" + c.number + ")";
    }

    // --- messages waiting on the phone ---
    private String messages(String app) {
        List<MessageListener.Message> list = MessageListener.messages(app);
        if (list == null) return "Error: The assistant has no access to the phone's notifications";
        if (list.isEmpty()) return "No new messages" + (app.isEmpty() ? "" : " in " + app);
        StringBuilder b = new StringBuilder();
        for (MessageListener.Message m : list) {
            b.append(m.app).append(" · ").append(m.sender).append(": ").append(m.text)
                    .append(m.reply == null ? " (no answer from here)" : "").append(" [key ").append(m.key).append("]\n");
        }
        return b.toString().trim();
    }

    private String reply(String key, String sender, String text) {
        if (text.isEmpty()) return "Error: the answer is missing";
        List<MessageListener.Message> list = MessageListener.messages("");
        if (list == null) return "Error: The assistant has no access to the phone's notifications";
        MessageListener.Message found = null;
        for (MessageListener.Message m : list) {
            if (!key.isEmpty() && m.key.equals(key)) {
                found = m;
                break;
            }
            if (key.isEmpty() && !sender.isEmpty() && m.sender.toLowerCase(Locale.ROOT).contains(sender.toLowerCase(Locale.ROOT))) {
                found = m;
                break;
            }
        }
        if (found == null) return "Error: no waiting message " + (key.isEmpty() ? "from " + sender : "with that key");
        String err = MessageListener.reply(found, text);
        return err.isEmpty() ? "Answered " + found.sender + " in " + found.app : err;
    }

    // --- media, apps, alarms ---
    private String media(String action) {
        int code;
        switch (action) {
            case "pause": code = KeyEvent.KEYCODE_MEDIA_PAUSE; break;
            case "play": code = KeyEvent.KEYCODE_MEDIA_PLAY; break;
            case "next": code = KeyEvent.KEYCODE_MEDIA_NEXT; break;
            case "previous": code = KeyEvent.KEYCODE_MEDIA_PREVIOUS; break;
            default: return "Error: pause, play, next or previous";
        }
        AudioManager am = ctx.getSystemService(AudioManager.class);
        am.dispatchMediaKeyEvent(new KeyEvent(KeyEvent.ACTION_DOWN, code));
        am.dispatchMediaKeyEvent(new KeyEvent(KeyEvent.ACTION_UP, code));
        return "Done: " + action;
    }

    private String openApp(String name) {
        PackageManager pm = ctx.getPackageManager();
        Intent launcher = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER);
        String want = name.toLowerCase(Locale.ROOT).trim();
        ResolveInfo best = null;
        for (ResolveInfo r : pm.queryIntentActivities(launcher, 0)) {
            String label = r.loadLabel(pm).toString().toLowerCase(Locale.ROOT);
            if (label.equals(want)) {
                best = r;
                break;
            }
            if (best == null && (label.contains(want) || want.contains(label))) best = r;
        }
        if (best == null) return "Error: no app \"" + name + "\" on the phone";
        Intent i = pm.getLaunchIntentForPackage(best.activityInfo.packageName);
        if (i == null) return "Error: " + name + " cannot be opened";
        String err = start(i);
        return err.isEmpty() ? best.loadLabel(pm) + " opened" : err;
    }

    private String alarm(String time, int seconds, String label) {
        Intent i;
        if (seconds > 0) {
            i = new Intent(AlarmClock.ACTION_SET_TIMER).putExtra(AlarmClock.EXTRA_LENGTH, seconds);
        } else if (time.matches("\\d{1,2}:\\d{2}")) {
            String[] hm = time.split(":");
            i = new Intent(AlarmClock.ACTION_SET_ALARM).putExtra(AlarmClock.EXTRA_HOUR, Integer.parseInt(hm[0]))
                    .putExtra(AlarmClock.EXTRA_MINUTES, Integer.parseInt(hm[1]));
        } else {
            return "Error: a time HH:MM or seconds";
        }
        i.putExtra(AlarmClock.EXTRA_SKIP_UI, true);
        if (!label.isEmpty()) i.putExtra(AlarmClock.EXTRA_MESSAGE, label);
        String err = start(i);
        if (!err.isEmpty()) return err;
        return seconds > 0 ? "Timer for " + seconds + " s set on the phone" : "Alarm at " + time + " set on the phone";
    }
}
