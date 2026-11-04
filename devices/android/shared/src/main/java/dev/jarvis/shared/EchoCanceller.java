package dev.jarvis.shared;

import android.util.Log;

// SpeexDSP's acoustic echo canceller and its preprocessor (cpp/jarvis_aec.c, built into the phone
// app only): Jarvis' own voice, as it was played, is taken off the microphone, so on the phone's
// loudspeaker he neither hears himself nor needs the call path (which sounded like a phone call),
// and the user can still interrupt him by voice. Frames of FRAME samples at 16 kHz.
public final class EchoCanceller {
    public static final int RATE = 16000, FRAME = 320; // 20 ms
    private static final int TAIL = 4096;              // 256 ms of echo path and timing slack

    private static final boolean available;

    static {
        boolean ok = false;
        try {
            System.loadLibrary("jarvis_aec");
            ok = true;
        } catch (Throwable e) {
            Log.i(Hud.TAG, "no echo canceller in this app: " + e);
        }
        available = ok;
    }

    private long handle;

    // create: an echo canceller, or null where the app has none (the Fire TV).
    public static EchoCanceller create() {
        if (!available) return null;
        EchoCanceller e = new EchoCanceller();
        e.handle = create(FRAME, TAIL, RATE);
        return e;
    }

    // process: mic and ref (what played as the mic would hear it), FRAME samples each, into out.
    public void process(short[] mic, short[] ref, short[] out) {
        process(handle, mic, ref, out);
    }

    public void reset() {
        reset(handle);
    }

    private static native long create(int frame, int tail, int rate);

    private static native void process(long h, short[] mic, short[] ref, short[] out);

    private static native void reset(long h);

    private static native void destroy(long h);
}
