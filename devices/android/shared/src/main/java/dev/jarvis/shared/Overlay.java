package dev.jarvis.shared;

import android.annotation.SuppressLint;
import android.content.Context;
import android.graphics.PixelFormat;
import android.os.Handler;
import android.os.Looper;
import android.util.DisplayMetrics;
import android.util.Log;
import android.view.Gravity;
import android.view.WindowManager;
import android.webkit.JavascriptInterface;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import org.json.JSONObject;

// A card in the top right corner above whatever app is in front (Netflix, YouTube, the launcher).
// It takes neither focus nor keys: the app below keeps the remote. One WebView, kept for reuse;
// card.html tells its height after rendering (Card.resize).
public final class Overlay {
    // The card is laid out 440 px wide (card.html) and drawn at SCALE: a quarter of the area it
    // had, close to the corner (29.10.2026: too large and too far in on the TVs).
    private static final int WIDTH_DP = 440, MARGIN_DP = 12, DEFAULT_SECONDS = 20;
    private static final float SCALE = 0.5f;

    private final Context ctx;
    private final WindowManager wm;
    private final Handler main = new Handler(Looper.getMainLooper());
    private final Runnable hideLater = this::hide;
    private final float density;
    private final int maxHeight;
    private WebView web;
    private VoiceLevel voice;
    private WindowManager.LayoutParams params;
    private boolean attached, loaded;
    private String pending;

    public Overlay(Context ctx) {
        this.ctx = ctx;
        wm = ctx.getSystemService(WindowManager.class);
        DisplayMetrics m = ctx.getResources().getDisplayMetrics();
        density = m.density;
        maxHeight = m.heightPixels - 2 * dp(MARGIN_DP);
    }

    private int dp(int value) {
        return Math.round(value * density);
    }

    @SuppressLint({"SetJavaScriptEnabled", "AddJavascriptInterface"})
    private void create() {
        web = new WebView(ctx);
        web.setBackgroundColor(0);
        web.getSettings().setJavaScriptEnabled(true);
        web.getSettings().setMediaPlaybackRequiresUserGesture(false);
        web.getSettings().setUseWideViewPort(true); // the page's width=440, scaled to the window
        web.getSettings().setLoadWithOverviewMode(true);
        web.setWebChromeClient(new ConsoleLog());
        web.addJavascriptInterface(new Object() {
            @JavascriptInterface
            public void resize(int cssHeight) {
                main.post(() -> setHeight(Math.min(Math.round(dp(cssHeight) * SCALE), maxHeight)));
            }
        }, "Card");
        web.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                loaded = true;
                if (pending != null) render(pending);
                pending = null;
            }
        });
        web.loadUrl("file:///android_asset/card.html?mode=overlay");
        voice = new VoiceLevel(web);
        params = new WindowManager.LayoutParams(Math.round(dp(WIDTH_DP) * SCALE), 1,
                WindowManager.LayoutParams.TYPE_APPLICATION_OVERLAY,
                WindowManager.LayoutParams.FLAG_NOT_FOCUSABLE | WindowManager.LayoutParams.FLAG_NOT_TOUCHABLE
                        | WindowManager.LayoutParams.FLAG_LAYOUT_IN_SCREEN,
                PixelFormat.TRANSLUCENT);
        params.gravity = Gravity.TOP | Gravity.END;
        params.x = dp(MARGIN_DP);
        params.y = dp(MARGIN_DP);
    }

    public void show(JSONObject card) {
        if (web == null) create();
        if (!attached) {
            try {
                wm.addView(web, params);
                attached = true;
            } catch (Exception e) {
                Log.e(Hud.TAG, "overlay not allowed (appops SYSTEM_ALERT_WINDOW?): " + e);
                return;
            }
        }
        voice.start();
        String json = card.toString();
        if (loaded) render(json);
        else pending = json;
        int seconds = card.optInt("seconds", 0);
        main.removeCallbacks(hideLater);
        main.postDelayed(hideLater, 1000L * (seconds > 0 ? seconds : DEFAULT_SECONDS));
    }

    private void render(String json) {
        web.evaluateJavascript("render(" + json + ")", null);
    }

    private void setHeight(int px) {
        if (!attached || params.height == px) return;
        params.height = px;
        wm.updateViewLayout(web, params);
    }

    public void hide() {
        main.removeCallbacks(hideLater);
        if (!attached) return;
        voice.stop();
        wm.removeView(web);
        attached = false;
        params.height = 1;
    }
}
