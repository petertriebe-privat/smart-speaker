package dev.smartspeaker.shared;

import android.util.Log;
import android.webkit.ConsoleMessage;
import android.webkit.WebChromeClient;

// card.html's console in logcat (adb logcat -s AssistantScreen): a broken image link shows there.
final class ConsoleLog extends WebChromeClient {
    @Override
    public boolean onConsoleMessage(ConsoleMessage m) {
        Log.i(Hud.TAG, "card: " + m.message());
        return true;
    }
}
