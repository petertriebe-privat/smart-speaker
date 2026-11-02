package dev.jarvis.phone.car;

import org.json.JSONObject;

import java.util.concurrent.CopyOnWriteArrayList;

// Jarvis' state and words for the car's screen: the status cards the core sends to the phone
// (PhoneService) are kept here, and the screen on the car redraws when they change.
public final class CarState {
    public static volatile String state = "idle", user = "", jarvis = "";
    private static final CopyOnWriteArrayList<Runnable> listeners = new CopyOnWriteArrayList<>();

    private CarState() {
    }

    // status: a status card ({"kind": "status", "state", "user", "jarvis"}).
    public static void status(JSONObject card) {
        String s = card.optString("state", "idle");
        String u = card.optString("user", ""), j = card.optString("jarvis", "");
        if (s.equals(state) && u.equals(user) && j.equals(jarvis)) return;
        if ("listening".equals(s) && !"listening".equals(state)) jarvis = ""; // a new question
        state = s;
        if (!u.isEmpty() || "listening".equals(s)) user = u;
        if (!j.isEmpty()) jarvis = j;
        for (Runnable r : listeners) r.run();
    }

    static void listen(Runnable r) {
        listeners.add(r);
    }

    static void unlisten(Runnable r) {
        listeners.remove(r);
    }
}
