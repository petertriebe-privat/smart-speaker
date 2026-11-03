package dev.jarvis.phone.car;

import org.json.JSONObject;

import java.util.concurrent.CopyOnWriteArrayList;

// Jarvis' state and words for the car's screen: the status cards the core sends to the phone
// (PhoneService) are kept here, with the last card he showed, and the screen on the car redraws
// when they change.
public final class CarState {
    public static volatile String state = "idle", user = "", jarvis = "";
    // card: the last card Jarvis showed (the HUD's JSON), until the next question; null: none
    static volatile String card;
    private static final CopyOnWriteArrayList<Runnable> listeners = new CopyOnWriteArrayList<>();

    private CarState() {
    }

    // status: a status card ({"kind": "status", "state", "user", "jarvis"}).
    public static void status(JSONObject card) {
        String s = card.optString("state", "idle");
        String u = card.optString("user", ""), j = card.optString("jarvis", "");
        if (s.equals(state) && u.equals(user) && j.equals(jarvis)) return;
        if ("listening".equals(s) && !"listening".equals(state)) { // a new question
            jarvis = "";
            CarState.card = null;
        }
        state = s;
        if (!u.isEmpty() || "listening".equals(s)) user = u;
        if (!j.isEmpty()) jarvis = j;
        changed();
    }

    // show: a card Jarvis shows (weather, calendar, a notification ...), a picture on the car's
    // screen until the next question.
    public static void show(JSONObject c) {
        try {
            c = new JSONObject(c.toString());
            c.remove("seconds");
            c.put("mode", "fullscreen");
        } catch (Exception ignored) {
        }
        card = c.toString();
        changed();
    }

    private static void changed() {
        for (Runnable r : listeners) r.run();
    }

    static void listen(Runnable r) {
        listeners.add(r);
    }

    static void unlisten(Runnable r) {
        listeners.remove(r);
    }
}
