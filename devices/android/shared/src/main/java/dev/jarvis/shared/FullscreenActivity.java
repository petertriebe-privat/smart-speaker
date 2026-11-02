package dev.jarvis.shared;

import android.annotation.SuppressLint;
import android.app.Activity;
import android.content.Intent;
import android.net.Uri;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;
import android.view.Gravity;
import android.view.View;
import android.view.WindowManager;
import android.webkit.JavascriptInterface;
import android.webkit.WebChromeClient;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.VideoView;

import org.json.JSONObject;

import java.lang.ref.WeakReference;

// The whole screen: the HUD (assets/card.html) with one card. It takes the remote: back closes it,
// as does the card's time or a clear from Jarvis; the app that was in front comes back.
// Two things the HUD page cannot do itself are laid over a slot it reports (Card.video, Card.web):
//   video  a native player: Amazon's WebView draws video behind the page, where the HUD covers it
//   web    a second WebView with the page ({"mode": "web", "url"}): many sites refuse iframes; the
//          remote scrolls it and follows links, back goes back in its history first. A web card
//          stays until closed unless it has seconds.
public class FullscreenActivity extends Activity {
    private static final String CARD_PAGE = "file:///android_asset/card.html?mode=fullscreen";
    // a card without its own time stays until back or the next one; a forgotten one closes after
    // this (29.10.2026: it closed after 2 min, right after the conversation)
    private static final int DEFAULT_SECONDS = 1800;
    private static WeakReference<FullscreenActivity> current = new WeakReference<>(null);

    // talk: the phone's Jarvis button (the phone app sets it): the page draws the button, a tap
    // or a hold goes to the core through the audio bridge. Null on the TV (the remote's key).
    public static volatile AudioBridge talk;

    private final Handler main = new Handler(Looper.getMainLooper());
    private FrameLayout root, videoBox;
    private WebView hud, page;
    private VoiceLevel voice;
    private VideoView video;
    private boolean loaded;
    private String pending;

    // close: Jarvis' clear. The console stays and shows itself again; anything else closes.
    public static void close() {
        FullscreenActivity a = current.get();
        if (a == null) return;
        if (a.console != null) a.main.post(() -> a.show(a.console));
        else a.finish();
    }

    @SuppressLint({"SetJavaScriptEnabled", "AddJavascriptInterface"})
    @Override
    protected void onCreate(Bundle state) {
        super.onCreate(state);
        current = new WeakReference<>(this);
        getWindow().addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON);
        root = new FrameLayout(this);
        hud = new WebView(this);
        hud.setBackgroundColor(0xFF050608);
        hud.getSettings().setJavaScriptEnabled(true);
        // An app, not a web page (the phone, 01.11.2026): no pinch zoom, Android's font size does
        // not grow the HUD's text (cards were cut), no text selection, no overscroll glow.
        hud.getSettings().setSupportZoom(false);
        hud.getSettings().setBuiltInZoomControls(false);
        hud.getSettings().setDisplayZoomControls(false);
        hud.getSettings().setTextZoom(100);
        hud.setOverScrollMode(View.OVER_SCROLL_NEVER);
        hud.setVerticalScrollBarEnabled(false);
        hud.setHorizontalScrollBarEnabled(false);
        hud.setLongClickable(false);
        hud.setOnLongClickListener(v -> true);
        hud.setHapticFeedbackEnabled(false);
        hud.setWebChromeClient(new ConsoleLog());
        hud.addJavascriptInterface(new Bridge(), "Card");
        hud.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                loaded = true;
                if (pending != null) render(pending);
                pending = null;
            }
        });
        root.addView(hud);
        setContentView(root);
        hud.loadUrl(CARD_PAGE + (talk != null ? "&talk=1" : ""));
        voice = new VoiceLevel(hud);
        show(getIntent().getStringExtra("card"));
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        show(intent.getStringExtra("card"));
    }

    // The console (a status card with "console": true, the app's home screen entry): it stays until
    // back, and a card shown meanwhile returns to it when its time is up.
    private String console;

    public static boolean consoleOpen() {
        FullscreenActivity a = current.get();
        return a != null && a.console != null && a.visible && !a.isFinishing();
    }

    // visible: in front. An app Jarvis started (YouTube, a film) covers it: then his state goes to
    // the corner instead of bringing the full screen back over the video (29.10.2026, a trailer
    // played behind it).
    private boolean visible;

    // immersive: the phone's HUD takes the whole screen; a swipe from the edge shows the system
    // bars for a moment.
    private void immersive() {
        if (talk == null || android.os.Build.VERSION.SDK_INT < 30) return;
        android.view.WindowInsetsController c = getWindow().getInsetsController();
        if (c == null) return;
        c.hide(android.view.WindowInsets.Type.systemBars());
        c.setSystemBarsBehavior(android.view.WindowInsetsController.BEHAVIOR_SHOW_TRANSIENT_BARS_BY_SWIPE);
    }

    @Override
    public void onWindowFocusChanged(boolean focus) {
        super.onWindowFocusChanged(focus);
        if (focus) immersive();
    }

    @Override
    protected void onStart() {
        super.onStart();
        visible = true;
        voice.start();
        AudioBridge t = talk;
        if (t != null) t.setAwake(true); // the phone: "Jarvis" is heard while the console is in front
    }

    @Override
    protected void onStop() {
        visible = false;
        voice.stop();
        AudioBridge t = talk;
        if (t != null) t.setAwake(false);
        super.onStop();
    }

    private void show(String json) {
        if (json == null) return;
        int seconds = 0;
        boolean web = false, status = false;
        try {
            JSONObject card = new JSONObject(json);
            seconds = card.optInt("seconds", 0);
            web = "web".equals(card.optString("mode"));
            status = "status".equals(card.optString("kind"));
            if (card.optBoolean("console")) console = json;
        } catch (Exception e) {
            return;
        }
        if (status && showingCard) {
            // Jarvis' state while a card is up: the page moves the avatar and the captions, the
            // card, its page or video and its time stay.
            if (loaded) render(json);
            return;
        }
        main.removeCallbacksAndMessages(null);
        stopLayers();
        showingCard = !status;
        if (loaded) render(json);
        else pending = json;
        if (status && console != null) return; // the console's own state: no time
        if (console != null) {
            // In the console a card stays until the next one or back, unless Jarvis gave it a time.
            if (seconds > 0) main.postDelayed(() -> show(console), 1000L * seconds);
            return;
        }
        if (seconds > 0 || !web) main.postDelayed(this::finish, 1000L * (seconds > 0 ? seconds : DEFAULT_SECONDS));
    }

    private boolean showingCard;

    public static boolean open() {
        FullscreenActivity a = current.get();
        return a != null && a.visible && !a.isFinishing();
    }

    private void render(String json) {
        hud.evaluateJavascript("render(" + json + ")", null);
    }

    // The slot's place in device pixels, as the page measured it.
    private static FrameLayout.LayoutParams at(int x, int y, int w, int h) {
        FrameLayout.LayoutParams lp = new FrameLayout.LayoutParams(w, h);
        lp.leftMargin = x;
        lp.topMargin = y;
        return lp;
    }

    @SuppressLint("SetJavaScriptEnabled")
    private void showPage(String url, int x, int y, int w, int h) {
        if (page == null) {
            page = new WebView(this);
            page.getSettings().setJavaScriptEnabled(true);
            page.getSettings().setDomStorageEnabled(true);
            page.getSettings().setMediaPlaybackRequiresUserGesture(false);
            page.setWebViewClient(new WebViewClient()); // links stay in the frame
            page.setWebChromeClient(new WebChromeClient() {
                @Override
                public void onReceivedTitle(WebView view, String title) {
                    // the page closed (about:blank) titled the next card "about:blank"
                    if (view.getVisibility() != View.VISIBLE || "about:blank".equals(view.getUrl())) return;
                    hud.evaluateJavascript("setPageTitle(" + JSONObject.quote(title) + ", "
                            + JSONObject.quote(view.getUrl()) + ")", null);
                }
            });
            root.addView(page);
        }
        page.setLayoutParams(at(x, y, w, h));
        page.setVisibility(View.VISIBLE);
        page.loadUrl(url);
        page.requestFocus();
    }

    private void showVideo(String url, int x, int y, int w, int h, boolean loop) {
        if (video == null) {
            videoBox = new FrameLayout(this); // black slot, the video centred in it at its own ratio
            videoBox.setBackgroundColor(0xFF000000);
            video = new VideoView(this);
            video.setZOrderMediaOverlay(true); // above the HUD's surface
            videoBox.addView(video, new FrameLayout.LayoutParams(FrameLayout.LayoutParams.WRAP_CONTENT,
                    FrameLayout.LayoutParams.WRAP_CONTENT, Gravity.CENTER));
            root.addView(videoBox);
        }
        videoBox.setLayoutParams(at(x, y, w, h));
        videoBox.setVisibility(View.VISIBLE);
        video.setVisibility(View.VISIBLE);
        video.setOnPreparedListener(mp -> mp.setLooping(loop));
        video.setOnErrorListener((mp, what, extra) -> {
            Log.w(Hud.TAG, "video failed (" + what + "/" + extra + "): " + url);
            videoBox.setVisibility(View.GONE);
            return true;
        });
        video.setVideoURI(Uri.parse(url));
        video.start();
    }

    private void stopLayers() {
        if (video != null) {
            video.stopPlayback();
            videoBox.setVisibility(View.GONE);
        }
        if (page != null) {
            page.loadUrl("about:blank");
            page.clearHistory();
            page.setVisibility(View.GONE);
        }
    }

    @Override
    public void onBackPressed() {
        if (page != null && page.getVisibility() == View.VISIBLE && page.canGoBack()) page.goBack();
        else if (console != null && showingCard) show(console); // from a card back to the console
        else finish();
    }

    @Override
    protected void onDestroy() {
        main.removeCallbacksAndMessages(null);
        if (current.get() == this) current = new WeakReference<>(null);
        Hud.closed(this); // e.g. a film paused for the card plays on
        stopLayers();
        if (page != null) page.destroy();
        hud.destroy();
        super.onDestroy();
    }

    // Called by assets/jarvis.js (on its own thread: everything goes to the main thread).
    private final class Bridge {
        @JavascriptInterface
        public void resize(int cssHeight) {
        }

        // The page's width in the app's pixels: the layers below are placed in them (jarvis.js
        // devicePixels; the WebView's devicePixelRatio was twice too large on the 4K TVs).
        @JavascriptInterface
        public int width() {
            return hud.getWidth();
        }

        @JavascriptInterface
        public void video(String url, int x, int y, int w, int h, boolean loop) {
            if (url.startsWith("https://") || url.startsWith("http://")) main.post(() -> showVideo(url, x, y, w, h, loop));
        }

        @JavascriptInterface
        public void web(String url, int x, int y, int w, int h) {
            if (url.startsWith("https://") || url.startsWith("http://")) main.post(() -> showPage(url, x, y, w, h));
        }

        // The phone's Jarvis button (jarvis.js talkButton).
        @JavascriptInterface
        public void press() {
            AudioBridge t = talk;
            if (t != null) t.press();
        }

        @JavascriptInterface
        public void release() {
            AudioBridge t = talk;
            if (t != null) t.release();
        }

        @JavascriptInterface
        public boolean listening() {
            AudioBridge t = talk;
            return t != null && t.listening();
        }
    }
}
