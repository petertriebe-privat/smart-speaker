package dev.jarvis.shared;

import android.util.Log;
import android.webkit.ConsoleMessage;
import android.webkit.WebChromeClient;

// card.html's console in logcat (adb logcat -s JarvisScreen): a broken image link shows there.
final class ConsoleLog extends WebChromeClient {
    @Override
    public boolean onConsoleMessage(ConsoleMessage m) {
        Log.i(Hud.TAG, "card: " + m.message());
        return true;
    }
}
