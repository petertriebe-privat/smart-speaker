package dev.jarvis.phone;

import android.app.Notification;
import android.app.PendingIntent;
import android.app.RemoteInput;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Bundle;
import android.os.Parcelable;
import android.service.notification.NotificationListenerService;
import android.service.notification.StatusBarNotification;

import java.util.ArrayList;
import java.util.List;
import java.util.Locale;

// The messages waiting on the phone (phone_messages, phone_reply): the notifications of the
// messengers, read and answered through their own reply action, as Android Auto does. The user
// grants notification access once (install_phone.sh does it over ADB). Listens to nothing while
// no tool asks: it only answers getActiveNotifications.
public class MessageListener extends NotificationListenerService {
    private static volatile MessageListener current;

    @Override
    public void onListenerConnected() {
        current = this;
    }

    @Override
    public void onListenerDisconnected() {
        if (current == this) current = null;
    }

    static final class Message {
        String key, app, sender, text;
        long when;
        Notification.Action reply;
    }

    // messages: the messenger notifications now, newest first; app filters by its name.
    static List<Message> messages(String app) {
        List<Message> out = new ArrayList<>();
        MessageListener l = current;
        if (l == null) return null;
        PackageManager pm = l.getPackageManager();
        StatusBarNotification[] all;
        try {
            all = l.getActiveNotifications();
        } catch (Exception e) {
            return null;
        }
        for (StatusBarNotification sbn : all) {
            Notification n = sbn.getNotification();
            if (sbn.getPackageName().equals(l.getPackageName())) continue;
            if ((n.flags & Notification.FLAG_GROUP_SUMMARY) != 0) continue;
            Notification.Action reply = replyAction(n);
            boolean message = Notification.CATEGORY_MESSAGE.equals(n.category) || reply != null
                    || n.extras.containsKey(Notification.EXTRA_MESSAGES);
            if (!message) continue;
            Message m = new Message();
            m.key = sbn.getKey();
            try {
                m.app = pm.getApplicationLabel(pm.getApplicationInfo(sbn.getPackageName(), 0)).toString();
            } catch (Exception e) {
                m.app = sbn.getPackageName();
            }
            if (app != null && !app.isEmpty() && !m.app.toLowerCase(Locale.ROOT).contains(app.toLowerCase(Locale.ROOT))) continue;
            CharSequence title = n.extras.getCharSequence(Notification.EXTRA_CONVERSATION_TITLE);
            if (title == null) title = n.extras.getCharSequence(Notification.EXTRA_TITLE);
            m.sender = title == null ? "" : title.toString();
            m.text = text(n);
            m.when = n.when;
            m.reply = reply;
            out.add(m);
        }
        out.sort((a, b) -> Long.compare(b.when, a.when));
        return out;
    }

    // text: the conversation's recent lines (MessagingStyle), else the notification's text.
    private static String text(Notification n) {
        Parcelable[] lines = n.extras.getParcelableArray(Notification.EXTRA_MESSAGES);
        if (lines != null && lines.length > 0) {
            StringBuilder b = new StringBuilder();
            for (int i = Math.max(0, lines.length - 6); i < lines.length; i++) {
                if (!(lines[i] instanceof Bundle)) continue;
                Bundle line = (Bundle) lines[i];
                CharSequence who = line.getCharSequence("sender"), what = line.getCharSequence("text");
                if (what == null) continue;
                if (b.length() > 0) b.append(" / ");
                if (who != null) b.append(who).append(": ");
                b.append(what);
            }
            if (b.length() > 0) return b.toString();
        }
        CharSequence big = n.extras.getCharSequence(Notification.EXTRA_BIG_TEXT);
        CharSequence t = big != null ? big : n.extras.getCharSequence(Notification.EXTRA_TEXT);
        return t == null ? "" : t.toString();
    }

    private static Notification.Action replyAction(Notification n) {
        if (n.actions == null) return null;
        Notification.Action any = null;
        for (Notification.Action a : n.actions) {
            if (a.getRemoteInputs() == null || a.getRemoteInputs().length == 0) continue;
            if (a.getSemanticAction() == Notification.Action.SEMANTIC_ACTION_REPLY) return a;
            if (any == null) any = a;
        }
        return any;
    }

    // reply answers m through its app's reply action; an error text or "".
    static String reply(Message m, String text) {
        MessageListener l = current;
        if (l == null) return "Error: no notification access";
        if (m.reply == null) return "Error: " + m.app + " offers no answer from its notification";
        Intent intent = new Intent();
        Bundle results = new Bundle();
        for (RemoteInput ri : m.reply.getRemoteInputs()) results.putCharSequence(ri.getResultKey(), text);
        RemoteInput.addResultsToIntent(m.reply.getRemoteInputs(), intent, results);
        try {
            m.reply.actionIntent.send(l, 0, intent);
            return "";
        } catch (PendingIntent.CanceledException e) {
            return "Error: the message is gone";
        }
    }
}
