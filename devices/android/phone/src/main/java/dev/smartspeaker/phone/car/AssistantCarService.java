package dev.smartspeaker.phone.car;

import android.content.Intent;
import android.net.Uri;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;

import androidx.annotation.NonNull;
import androidx.car.app.CarAppService;
import androidx.car.app.CarContext;
import androidx.car.app.Screen;
import androidx.car.app.Session;
import androidx.car.app.validation.HostValidator;
import androidx.lifecycle.DefaultLifecycleObserver;
import androidx.lifecycle.LifecycleOwner;

import dev.smartspeaker.phone.PhoneService;
import dev.smartspeaker.shared.Hud;

import java.lang.ref.WeakReference;

// Android Auto (#75): The assistant on the car's screen. A Car App Library app in the IoT category (there
// is none for assistants); sideloaded, so any host is let in (Android Auto's "Unknown sources").
// Opening it starts the phone's assistant if it does not run yet. While its session lives, navigation
// goes to the car's navigation app (phone_navigate).
public final class AssistantCarService extends CarAppService {
    private static volatile WeakReference<CarContext> car = new WeakReference<>(null);

    // navigate: the car's navigation app to destination; false without a session on the car.
    public static boolean navigate(String destination) {
        CarContext c = car.get();
        if (c == null) return false;
        new Handler(Looper.getMainLooper()).post(() -> {
            try {
                c.startCarApp(new Intent(CarContext.ACTION_NAVIGATE, Uri.parse("geo:0,0?q=" + Uri.encode(destination))));
            } catch (Exception e) {
                Log.w(Hud.TAG, "car navigation: " + e);
            }
        });
        return true;
    }

    @NonNull
    @Override
    public HostValidator createHostValidator() {
        return HostValidator.ALLOW_ALL_HOSTS_VALIDATOR;
    }

    @NonNull
    @Override
    public Session onCreateSession() {
        Session s = new Session() {
            @NonNull
            @Override
            public Screen onCreateScreen(@NonNull Intent intent) {
                try {
                    getCarContext().startForegroundService(new Intent(getCarContext(), PhoneService.class));
                } catch (Exception ignored) {
                    // already running, or not allowed from here: the phone's app starts it
                }
                car = new WeakReference<>(getCarContext());
                return new AssistantScreen(getCarContext());
            }
        };
        s.getLifecycle().addObserver(new DefaultLifecycleObserver() {
            @Override
            public void onDestroy(@NonNull LifecycleOwner owner) {
                if (car.get() == s.getCarContext()) car = new WeakReference<>(null);
            }
        });
        return s;
    }
}
