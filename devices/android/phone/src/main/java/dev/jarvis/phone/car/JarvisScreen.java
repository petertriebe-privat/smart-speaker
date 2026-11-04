package dev.jarvis.phone.car;

import android.graphics.Bitmap;
import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;
import android.util.Log;

import androidx.annotation.NonNull;
import androidx.car.app.AppManager;
import androidx.car.app.CarContext;
import androidx.car.app.Screen;
import androidx.car.app.model.Action;
import androidx.car.app.model.CarColor;
import androidx.car.app.model.CarIcon;
import androidx.car.app.model.MessageTemplate;
import androidx.car.app.model.Pane;
import androidx.car.app.model.PaneTemplate;
import androidx.car.app.model.Row;
import androidx.car.app.model.Template;
import androidx.car.app.navigation.model.MapWithContentTemplate;
import androidx.core.graphics.drawable.IconCompat;
import androidx.lifecycle.DefaultLifecycleObserver;
import androidx.lifecycle.LifecycleOwner;

import dev.jarvis.phone.PhoneCore;
import dev.jarvis.phone.R;
import dev.jarvis.shared.AudioBridge;
import dev.jarvis.shared.Hud;

import org.json.JSONObject;

// The car's screen, as the phone's HUD within what Android Auto allows. Two forms, by what the car
// can (its car API level):
//   7+  live (MapWithContentTemplate, as a weather app may): the HUD fills the screen behind a small
//       panel (CarSurface), his hologram moving in its state's colour with the words of the turn,
//       or the card he showed in full size; the panel holds his state and the buttons.
//   4+  pictures (PaneTemplate): his state, the user's last question and his answer as rows, and
//       a picture of the card or the hologram drawn off screen (CardRenderer).
// "Sprechen" starts a conversation (hands free, as a tap on the phone; or say "Jarvis"), and while
// one runs "Unterbrechen" stops him while he speaks and "Beenden" ends it. The template is redrawn
// at most once a second (the car limits how often a template may change); the live HUD at once.
final class JarvisScreen extends Screen implements DefaultLifecycleObserver {
    private static final long REDRAW_MS = 1000;
    private static final int PICTURE_API = 4; // Pane.setImage
    private static final int LIVE_API = 7; // MapWithContentTemplate
    private final Handler main = new Handler(Looper.getMainLooper());
    private long drawn;
    private boolean pending;
    private final Runnable changed = () -> main.post(this::update);
    private CarSurface surface;
    private CardRenderer renderer;
    private String drawing; // what the picture shows: the card's JSON or the hologram's state
    private Bitmap picture;
    private String panel = ""; // the panel's content last drawn: only a change redraws it

    JarvisScreen(@NonNull CarContext ctx) {
        super(ctx);
        getLifecycle().addObserver(this);
    }

    private boolean live() {
        return getCarContext().getCarAppApiLevel() >= LIVE_API;
    }

    @Override
    public void onStart(@NonNull LifecycleOwner owner) {
        CarState.listen(changed);
        try {
            if (live()) {
                surface = new CarSurface(getCarContext());
                getCarContext().getCarService(AppManager.class).setSurfaceCallback(surface);
            } else if (getCarContext().getCarAppApiLevel() >= PICTURE_API) {
                renderer = new CardRenderer(getCarContext());
            }
        } catch (Exception e) {
            Log.w(Hud.TAG, "car screen: " + e);
        }
        update();
    }

    @Override
    public void onStop(@NonNull LifecycleOwner owner) {
        CarState.unlisten(changed);
        main.removeCallbacksAndMessages(null);
        if (surface != null) {
            getCarContext().getCarService(AppManager.class).setSurfaceCallback(null);
            surface.release();
        }
        surface = null;
        if (renderer != null) renderer.release();
        renderer = null;
        drawing = null;
    }

    // update: the live HUD or a new picture when the card or the state changed, and the template.
    private void update() {
        String card = CarState.card;
        if (surface != null) {
            surface.show(card != null ? card : status(true));
            String p = CarState.state + "|" + PhoneCore.audio(getCarContext()).inConversation();
            if (p.equals(panel)) return;
            panel = p;
        } else if (renderer != null) {
            String want = card != null ? card : status(false);
            if (!want.equals(drawing)) {
                drawing = want;
                renderer.render(want, b -> {
                    if (!want.equals(drawing)) return;
                    picture = b;
                    redraw();
                });
            }
        }
        redraw();
    }

    // status: the HUD with Jarvis alone, in his state's colour, with the turn's words if wanted.
    private static String status(boolean words) {
        JSONObject s = new JSONObject();
        try {
            s.put("kind", "status");
            s.put("state", CarState.state);
            if (words) {
                s.put("user", CarState.user);
                s.put("jarvis", CarState.jarvis);
            }
            s.put("mode", "fullscreen");
        } catch (Exception ignored) {
        }
        return s.toString();
    }

    private void redraw() {
        long wait = drawn + REDRAW_MS - SystemClock.uptimeMillis();
        if (wait > 0) {
            if (!pending) {
                pending = true;
                main.postDelayed(() -> {
                    pending = false;
                    redraw();
                }, wait);
            }
            return;
        }
        drawn = SystemClock.uptimeMillis();
        invalidate();
    }

    private static String label(String state) {
        switch (state) {
            case "listening":
                return "Ich höre";
            case "thinking":
                return "Einen Moment …";
            case "speaking":
                return "Jarvis spricht";
            default:
                return "Bereit";
        }
    }

    // actions: "Sprechen", or while a conversation runs "Unterbrechen" (while he speaks) and "Beenden".
    private Action[] actions(AudioBridge audio) {
        if (!audio.inConversation()) {
            return new Action[]{new Action.Builder().setTitle("Sprechen").setBackgroundColor(CarColor.PRIMARY)
                    .setOnClickListener(audio::talk).build()};
        }
        Action end = new Action.Builder().setTitle("Beenden").setOnClickListener(audio::end).build();
        if (!"speaking".equals(CarState.state)) return new Action[]{end};
        return new Action[]{new Action.Builder().setTitle("Unterbrechen")
                .setOnClickListener(audio::interrupt).build(), end};
    }

    @NonNull
    @Override
    @androidx.annotation.OptIn(markerClass = androidx.car.app.annotations.ExperimentalCarApi.class)
    public Template onGetTemplate() {
        AudioBridge audio = PhoneCore.audio(getCarContext());
        CarIcon icon = new CarIcon.Builder(IconCompat.createWithResource(getCarContext(), R.drawable.icon)).build();
        if (surface != null) {
            MessageTemplate.Builder m = new MessageTemplate.Builder(label(CarState.state))
                    .setTitle("Jarvis").setHeaderAction(Action.APP_ICON);
            for (Action a : actions(audio)) m.addAction(a);
            return new MapWithContentTemplate.Builder().setContentTemplate(m.build()).build();
        }
        Pane.Builder pane = new Pane.Builder();
        pane.addRow(new Row.Builder().setTitle(label(CarState.state)).build());
        if (!CarState.user.isEmpty()) {
            pane.addRow(new Row.Builder().setTitle("„" + CarState.user + "“").build());
        }
        if (!CarState.jarvis.isEmpty()) {
            pane.addRow(new Row.Builder().setTitle("Jarvis").addText(CarState.jarvis).build());
        }
        if (getCarContext().getCarAppApiLevel() >= PICTURE_API) {
            pane.setImage(picture != null ? new CarIcon.Builder(IconCompat.createWithBitmap(picture)).build() : icon);
        }
        for (Action a : actions(audio)) pane.addAction(a);
        return new PaneTemplate.Builder(pane.build())
                .setTitle("Jarvis")
                .setHeaderAction(Action.APP_ICON)
                .build();
    }
}
