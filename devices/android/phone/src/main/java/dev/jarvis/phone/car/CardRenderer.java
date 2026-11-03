package dev.jarvis.phone.car;

import android.annotation.SuppressLint;
import android.app.Presentation;
import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.PixelFormat;
import android.hardware.display.DisplayManager;
import android.hardware.display.VirtualDisplay;
import android.media.Image;
import android.media.ImageReader;
import android.os.Handler;
import android.os.Looper;
import android.util.Log;
import android.webkit.WebView;
import android.webkit.WebViewClient;

import dev.jarvis.shared.Hud;

import org.json.JSONArray;

import java.nio.ByteBuffer;

// The HUD as pictures for the car's screen (Android Auto lets a non-navigation app show templates
// only, no drawing of its own): the same page as on the phone and the TV (card.html, full screen,
// car=1: without bars and avatar) runs on a private virtual display of the app, unseen, and its
// last frame is cut to the card, or to his hologram in its state, and handed on as a bitmap.
final class CardRenderer {
    static final int W = 960, H = 540;
    private static final int DPI = 160; // CSS pixels = the display's pixels
    private static final int MARGIN = 12;
    private static final long SETTLE_MS = 900; // the page draws the card, pictures load
    private static final long FIRST_SETTLE_MS = 3000; // the first time the 3D hologram starts too (black before)

    interface Done {
        void bitmap(Bitmap b);
    }

    private final Handler main = new Handler(Looper.getMainLooper());
    private final ImageReader reader;
    private final VirtualDisplay display;
    private final WebView page;
    private Image last; // the newest frame: a still page sends no new ones
    private boolean loaded, first = true;
    private String pending;
    private Done pendingDone;
    private int generation;

    @SuppressLint("SetJavaScriptEnabled")
    CardRenderer(Context ctx) {
        reader = ImageReader.newInstance(W, H, PixelFormat.RGBA_8888, 2);
        reader.setOnImageAvailableListener(r -> {
            Image img = r.acquireNextImage();
            if (img == null) return;
            if (last != null) last.close();
            last = img;
        }, main);
        DisplayManager dm = ctx.getSystemService(DisplayManager.class);
        display = dm.createVirtualDisplay("jarvis-car", W, H, DPI, reader.getSurface(), 0);
        Presentation p = new Presentation(ctx, display.getDisplay());
        page = new WebView(p.getContext());
        page.setBackgroundColor(0xFF050608);
        page.getSettings().setJavaScriptEnabled(true);
        page.getSettings().setTextZoom(100);
        page.setWebViewClient(new WebViewClient() {
            @Override
            public void onPageFinished(WebView view, String url) {
                loaded = true;
                if (pending != null) render(pending, pendingDone);
                pending = null;
                pendingDone = null;
            }
        });
        p.setContentView(page);
        p.show();
        page.loadUrl("file:///android_asset/card.html?mode=fullscreen&car=1");
    }

    // render: card (the HUD's JSON) drawn, then its picture to done on the main thread; a newer
    // render supersedes one still settling.
    void render(String card, Done done) {
        if (!loaded) {
            pending = card;
            pendingDone = done;
            return;
        }
        int gen = ++generation;
        page.evaluateJavascript("render(" + card + ")", null);
        main.postDelayed(() -> {
            if (gen != generation) return;
            page.evaluateJavascript("JSON.stringify(carRect())", rect -> {
                if (gen != generation) return;
                Bitmap b = capture(rect);
                if (b != null) done.bitmap(b);
            });
        }, first ? FIRST_SETTLE_MS : SETTLE_MS);
        first = false;
    }

    // capture: the last frame cut to rect ([x, y, w, h] as JSON from the page, quoted) with a
    // margin, widened to 16:9, the car's picture; the whole frame without one.
    private Bitmap capture(String rect) {
        if (last == null) return null;
        try {
            Image.Plane plane = last.getPlanes()[0];
            ByteBuffer buf = plane.getBuffer();
            buf.rewind();
            int pixel = plane.getPixelStride(), row = plane.getRowStride();
            Bitmap wide = Bitmap.createBitmap(row / pixel, H, Bitmap.Config.ARGB_8888);
            wide.copyPixelsFromBuffer(buf);
            int[] r = cut(rect);
            Bitmap b = Bitmap.createBitmap(wide, r[0], r[1], r[2], r[3]);
            if (b != wide) wide.recycle();
            return b;
        } catch (Exception e) {
            Log.w(Hud.TAG, "car picture: " + e);
            return null;
        }
    }

    private static int[] cut(String rect) {
        try {
            // evaluateJavascript hands the string back as a JSON string literal
            JSONArray a = new JSONArray(new JSONArray("[" + rect + "]").getString(0));
            double x = a.getDouble(0) - MARGIN, y = a.getDouble(1) - MARGIN;
            double w = a.getDouble(2) + 2 * MARGIN, h = a.getDouble(3) + 2 * MARGIN;
            if (w * 9 > h * 16) { // too wide: taller
                double nh = w * 9 / 16;
                y -= (nh - h) / 2;
                h = nh;
            } else { // too tall: wider
                double nw = h * 16 / 9;
                x -= (nw - w) / 2;
                w = nw;
            }
            int cw = (int) Math.min(W, Math.round(w)), ch = (int) Math.min(H, Math.round(h));
            int cx = (int) Math.max(0, Math.min(W - cw, Math.round(x)));
            int cy = (int) Math.max(0, Math.min(H - ch, Math.round(y)));
            if (cw < 32 || ch < 18) return new int[]{0, 0, W, H};
            return new int[]{cx, cy, cw, ch};
        } catch (Exception e) {
            return new int[]{0, 0, W, H};
        }
    }

    void release() {
        generation++;
        page.destroy();
        display.release();
        if (last != null) last.close();
        last = null;
        reader.close();
    }
}
