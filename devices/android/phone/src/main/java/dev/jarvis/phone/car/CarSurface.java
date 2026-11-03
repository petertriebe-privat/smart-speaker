package dev.jarvis.phone.car;

import android.annotation.SuppressLint;
import android.app.Presentation;
import android.graphics.Rect;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.util.Log;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import androidx.annotation.NonNull;
import androidx.car.app.CarContext;
import androidx.car.app.SurfaceCallback;
import androidx.car.app.SurfaceContainer;

import dev.jarvis.shared.Hud;

// The HUD live on the car's screen: as a weather app (car API 7+, MapWithContentTemplate) Jarvis
// gets the screen's surface behind a small panel, and the phone's HUD page (card.html, car=1:
// without bars and avatar) runs on a virtual display drawn straight into it: his hologram moving
// in its state's colour, or the card he showed, in full size beside the panel.
final class CarSurface implements SurfaceCallback {
    private final CarContext ctx;
    private VirtualDisplay display;
    private Presentation presentation;
    private WebView page;
    private boolean loaded;
    private String card; // the last card or status, drawn once the page has loaded
    private Rect visible;
    private float density = 1;

    CarSurface(CarContext ctx) {
        this.ctx = ctx;
    }

    @SuppressLint("SetJavaScriptEnabled")
    @Override
    public void onSurfaceAvailable(@NonNull SurfaceContainer s) {
        release();
        if (s.getSurface() == null || s.getWidth() <= 0 || s.getHeight() <= 0) return;
        density = Math.max(1, s.getDpi()) / 160f;
        DisplayManager dm = ctx.getSystemService(DisplayManager.class);
        display = dm.createVirtualDisplay("jarvis-car", s.getWidth(), s.getHeight(), s.getDpi(), s.getSurface(), 0);
        presentation = new Presentation(ctx, display.getDisplay());
        page = new WebView(presentation.getContext());
        page.setBackgroundColor(0xFF050608);
        page.getSettings().setJavaScriptEnabled(true);
        page.getSettings().setTextZoom(100);
        page.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                loaded = true;
                area();
                if (card != null) show(card);
            }
        });
        presentation.setContentView(page);
        try {
            presentation.show();
        } catch (Exception e) {
            Log.w(Hud.TAG, "car surface: " + e);
            release();
            return;
        }
        page.loadUrl("file:///android_asset/card.html?mode=fullscreen&car=1");
        Log.i(Hud.TAG, "car surface " + s.getWidth() + "x" + s.getHeight() + " at " + s.getDpi() + " dpi");
    }

    // onVisibleAreaChanged: the part of the screen beside the panel; the HUD fits itself into it.
    @Override
    public void onVisibleAreaChanged(@NonNull Rect r) {
        visible = new Rect(r);
        area();
    }

    @Override
    public void onSurfaceDestroyed(@NonNull SurfaceContainer s) {
        release();
    }

    // show: a card or a status (the HUD's JSON) on the car's screen.
    void show(String json) {
        card = json;
        if (page != null && loaded) page.evaluateJavascript("render(" + json + ")", null);
    }

    private void area() {
        if (page == null || !loaded || visible == null || visible.isEmpty()) return;
        page.evaluateJavascript(String.format(java.util.Locale.ROOT, "carArea(%.1f, %.1f, %.1f, %.1f)",
                visible.left / density, visible.top / density, visible.width() / density,
                visible.height() / density), null);
    }

    void release() {
        loaded = false;
        if (page != null) page.destroy();
        page = null;
        if (presentation != null) presentation.dismiss();
        presentation = null;
        if (display != null) display.release();
        display = null;
    }
}
