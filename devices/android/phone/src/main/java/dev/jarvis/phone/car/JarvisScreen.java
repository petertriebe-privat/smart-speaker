package dev.jarvis.phone.car;

import android.os.Handler;
import android.os.Looper;
import android.os.SystemClock;

import androidx.annotation.NonNull;
import androidx.car.app.CarContext;
import androidx.car.app.Screen;
import androidx.car.app.model.Action;
import androidx.car.app.model.CarColor;
import androidx.car.app.model.CarIcon;
import androidx.car.app.model.MessageTemplate;
import androidx.car.app.model.Template;
import androidx.core.graphics.drawable.IconCompat;
import androidx.lifecycle.DefaultLifecycleObserver;
import androidx.lifecycle.LifecycleOwner;

import dev.jarvis.phone.PhoneCore;
import dev.jarvis.phone.R;
import dev.jarvis.shared.AudioBridge;

// The car's screen: Jarvis' state, the user's last question and his answer, and two buttons:
// "Sprechen" starts a conversation (hands free, as a tap on the phone; or says "Jarvis"), and while
// one runs "Unterbrechen" stops him while he speaks and "Beenden" ends it. Redrawn when his state
// or words change, at most once a second (the car limits how often a template may change).
final class JarvisScreen extends Screen implements DefaultLifecycleObserver {
    private static final long REDRAW_MS = 1000;
    private final Handler main = new Handler(Looper.getMainLooper());
    private long drawn;
    private boolean pending;
    private final Runnable changed = () -> main.post(this::redraw);

    JarvisScreen(@NonNull CarContext ctx) {
        super(ctx);
        getLifecycle().addObserver(this);
    }

    @Override
    public void onStart(@NonNull LifecycleOwner owner) {
        CarState.listen(changed);
    }

    @Override
    public void onStop(@NonNull LifecycleOwner owner) {
        CarState.unlisten(changed);
        main.removeCallbacksAndMessages(null);
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
                return "Bereit. Sagen Sie „Jarvis“ oder tippen Sie auf Sprechen.";
        }
    }

    @NonNull
    @Override
    public Template onGetTemplate() {
        AudioBridge audio = PhoneCore.audio(getCarContext());
        StringBuilder text = new StringBuilder(label(CarState.state));
        if (!CarState.user.isEmpty()) text.append("\n\n„").append(CarState.user).append("“");
        if (!CarState.jarvis.isEmpty()) text.append("\n\n").append(CarState.jarvis);
        CarIcon icon = new CarIcon.Builder(IconCompat.createWithResource(getCarContext(), R.drawable.icon)).build();
        MessageTemplate.Builder t = new MessageTemplate.Builder(text.toString())
                .setTitle("Jarvis")
                .setHeaderAction(Action.APP_ICON)
                .setIcon(icon);
        if (audio.inConversation()) {
            if ("speaking".equals(CarState.state)) {
                t.addAction(new Action.Builder().setTitle("Unterbrechen")
                        .setOnClickListener(audio::interrupt).build());
            }
            t.addAction(new Action.Builder().setTitle("Beenden")
                    .setOnClickListener(audio::end).build());
        } else {
            t.addAction(new Action.Builder().setTitle("Sprechen").setBackgroundColor(CarColor.PRIMARY)
                    .setOnClickListener(audio::talk).build());
        }
        return t.build();
    }
}
