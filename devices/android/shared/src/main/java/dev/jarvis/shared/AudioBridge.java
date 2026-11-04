package dev.jarvis.shared;

import android.annotation.SuppressLint;
import android.content.Context;
import android.media.AudioAttributes;
import android.media.AudioDeviceInfo;
import android.media.AudioFormat;
import android.media.AudioManager;
import android.media.AudioRecord;
import android.media.AudioTrack;
import android.media.MediaRecorder;
import android.os.Build;
import android.util.Log;

import java.io.BufferedInputStream;
import java.io.ByteArrayOutputStream;
import java.io.IOException;
import java.io.InputStream;
import java.io.OutputStream;
import java.net.InetAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.nio.charset.StandardCharsets;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.TimeUnit;

// Microphone and speaker of the Jarvis core running on this device (core/internal/audio/tv.go):
// a TCP server on 127.0.0.1:PORT, one connection each.
//   "mic\n"                    16 kHz s16le mono in real time: the remote's microphone while its
//                              key is held (Fire OS streams it only then), exact silence otherwise
//   "play <rate> <ch> <name>\n" then s16le samples into an AudioTrack; closing the connection
//                              stops the playback at once (Jarvis is interrupted)
//   "keys\n"                   the phone's Jarvis button (core: cmd/assistant/phoneboard.go):
//                              "tap", "hold 1", "hold 0" to Jarvis; "mic 1" / "mic 0" back while a
//                              conversation needs the microphone
// On the phone (forPhone) the microphone is open only while the button is held or a conversation
// runs, through the phone's echo cancellation (VOICE_COMMUNICATION), and Jarvis speaks on the
// communication device meanwhile: the earbuds, the car or the loudspeaker.
public final class AudioBridge {
    public static final int PORT = 47160;
    private static final int MIC_RATE = 16000, CHUNK = 320; // 20 ms

    private final ArrayBlockingQueue<short[]> heard = new ArrayBlockingQueue<>(150);

    // level: the loudness of what Jarvis says or hears right now (0..1): the hologram pulses with it
    // as the Echo's ring does (FullscreenActivity reads it).
    public static volatile float level;

    private static void measure(short[] pcm, int n) {
        int peak = 0;
        for (int i = 0; i < n; i++) peak = Math.max(peak, Math.abs(pcm[i]));
        level = Math.max(level * 0.6f, Math.min(1f, peak / 20000f));
    }

    private static void measure(byte[] b, int n) {
        int peak = 0;
        for (int i = 0; i + 1 < n; i += 2) peak = Math.max(peak, Math.abs((short) ((b[i] & 0xff) | (b[i + 1] << 8))));
        level = Math.max(level * 0.6f, Math.min(1f, peak / 20000f));
    }

    private volatile boolean running;
    private ServerSocket server;

    private final AudioManager am; // the phone only: its microphone follows the button
    private volatile OutputStream keys;
    // holding: the button is down; holdTalk: this conversation was started by holding, so the
    // microphone follows the button (push to talk), not the conversation; coreMic: Jarvis' "mic 1";
    // open: the microphone is open now
    private volatile boolean holding, holdTalk, coreMic, open;

    public AudioBridge() {
        am = null;
    }

    private AudioBridge(Context ctx) {
        am = ctx.getSystemService(AudioManager.class);
    }

    public static AudioBridge forPhone(Context ctx) {
        return new AudioBridge(ctx);
    }

    // The phone's button. Tapped: a conversation starts, Jarvis hears when the user is done and
    // the microphone stays open; tapped again, it ends. Held: push to talk, the microphone open
    // only while it is down. A press opens the microphone at once (the first word must not be
    // lost); its release tells which it was.
    private static final long TAP_MS = 350;
    private long pressedAt;
    private boolean pressedFree;  // pressed during a conversation started by a tap
    private boolean pressedStart; // pressed with no conversation: this press starts one

    public void press() {
        pressedAt = System.currentTimeMillis();
        pressedFree = coreMic && !holdTalk;
        pressedStart = !coreMic;
        if (pressedFree) return; // the microphone is open anyway; the release decides
        holding = true;
        holdTalk = true;
        focus(true);
        send("hold 1");
    }

    // focus: the phone's other audio (Spotify, a podcast) pauses for the conversation and plays on
    // after it, as for the navigation's announcements: Android's transient audio focus, held from
    // the press or the conversation's start until it ends.
    private android.media.AudioFocusRequest focusRequest;

    private synchronized void focus(boolean on) {
        if (am == null) return;
        if (on && focusRequest == null) {
            focusRequest = new android.media.AudioFocusRequest.Builder(AudioManager.AUDIOFOCUS_GAIN_TRANSIENT)
                    .setAudioAttributes(new AudioAttributes.Builder().setUsage(AudioAttributes.USAGE_ASSISTANT)
                            .setContentType(AudioAttributes.CONTENT_TYPE_SPEECH).build())
                    .build();
            am.requestAudioFocus(focusRequest);
        } else if (!on && focusRequest != null) {
            am.abandonAudioFocusRequest(focusRequest);
            focusRequest = null;
        }
    }

    public void release() {
        boolean tap = System.currentTimeMillis() - pressedAt < TAP_MS;
        if (pressedFree) {
            if (!tap) return;
            if (am != null && !callAudio && speaking()) barge(); // stops Jarvis, he listens
            else send("tap");                                     // ends the conversation
            return;
        }
        holding = false;
        send("hold 0");
        // a press that started no conversation (a ringing timer stopped) gives the focus back
        thread("jarvis-focus", () -> {
            sleep(6000);
            if (!coreMic && !holding) focus(false);
        });
        if (!tap) return;
        if (pressedStart) {
            // a tap started it: hands free on GPT Live ("free"; "hold 1" chose push to talk), the
            // microphone follows the conversation and stays open until Jarvis says it is up
            send("free");
            holdTalk = false;
            awaitUntil = System.currentTimeMillis() + 5000;
        } else {
            send("tap"); // a tap in a push-to-talk conversation ends it
        }
    }

    private volatile long awaitUntil;

    // listening: the phone's microphone is open.
    public boolean listening() {
        return open;
    }

    private void send(String line) {
        OutputStream out = keys;
        if (out == null) {
            Log.w(Hud.TAG, "button: Jarvis is not connected");
            return;
        }
        thread("jarvis-key", () -> {
            try {
                synchronized (out) {
                    out.write((line + "\n").getBytes(StandardCharsets.UTF_8));
                    out.flush();
                }
            } catch (IOException e) {
                Log.w(Hud.TAG, "button: " + e);
            }
        });
    }

    private boolean micWanted() {
        return am == null || awake || car || conversation();
    }

    // conversation: the microphone is for Jarvis now (held, or a conversation that needs it), with
    // the phone's echo cancellation; otherwise only the wake word listens.
    private boolean conversation() {
        return holding || coreMic && !holdTalk || System.currentTimeMillis() < awaitUntil;
    }

    // awake: the console is open in front, or the phone is in the car (Android Auto): the
    // microphone stays open for the wake word ("Jarvis").
    private volatile boolean awake, car;

    public void setAwake(boolean on) {
        awake = on;
    }

    public void setCar(boolean on) {
        if (on != car) Log.i(Hud.TAG, on ? "Android Auto connected: listening for Jarvis" : "Android Auto gone");
        car = on;
    }

    // For the car's screen (phone: car/JarvisScreen).
    public boolean inConversation() {
        return coreMic;
    }

    // talk: a conversation as a tap on the phone's button starts one (hands free).
    public void talk() {
        press();
        release();
    }

    // end: the conversation ends.
    public void end() {
        if (coreMic) send("tap");
    }

    // interrupt: Jarvis stops speaking and listens.
    public void interrupt() {
        barge();
    }

    // Cards: Jarvis' state and words from the core ("card {…}" on the button connection).
    public interface Cards {
        void card(String json);
    }

    public volatile Cards onCard;

    // Tools: the phone's tools (core: cmd/assistant/phonetools.go): "tool <id> {name, args}" on the
    // button connection; the answer goes back with result.
    public interface Tools {
        void run(String id, String json);
    }

    public volatile Tools onTool;

    public void result(String id, String text) {
        send("result " + id + " " + org.json.JSONObject.quote(text));
    }

    // generation: each start's threads end when the next start comes. A stop and a quick start
    // (the setup restarting the service) left the old recorder running beside the new one: two
    // microphones fed the core (01.11.2026).
    private volatile int generation;

    public void start() throws IOException {
        running = true;
        int gen = ++generation;
        server = new ServerSocket(PORT, 8, InetAddress.getByName("127.0.0.1"));
        thread("jarvis-audio-accept", this::accept);
        thread("jarvis-audio-record", () -> record(gen));
    }

    public void stop() {
        running = false;
        try {
            server.close();
        } catch (Exception ignored) {
        }
    }

    private static void thread(String name, Runnable r) {
        Thread t = new Thread(r, name);
        t.setDaemon(true);
        t.start();
    }

    private void accept() {
        while (running) {
            try {
                Socket s = server.accept();
                thread("jarvis-audio-client", () -> serve(s));
            } catch (IOException e) {
                if (running) Log.w(Hud.TAG, "audio accept: " + e);
            }
        }
    }

    private void serve(Socket s) {
        try (Socket socket = s) {
            socket.setTcpNoDelay(true);
            InputStream in = new BufferedInputStream(socket.getInputStream());
            String[] head = line(in).trim().split(" ");
            if (head[0].equals("mic")) {
                serveMic(socket.getOutputStream());
            } else if (head[0].equals("keys")) {
                serveKeys(in, socket.getOutputStream());
            } else if (head[0].equals("play") && head.length >= 3) {
                play(in, Integer.parseInt(head[1]), Integer.parseInt(head[2]), head.length > 3 ? head[3] : "");
            }
        } catch (Exception e) {
            if (running) Log.i(Hud.TAG, "audio client ended: " + e);
        }
    }

    // line: one line without its end; null at the end of the stream.
    private static String readLine(InputStream in) throws IOException {
        ByteArrayOutputStream b = new ByteArrayOutputStream();
        int c;
        while ((c = in.read()) != -1 && c != '\n') b.write(c);
        if (c == -1 && b.size() == 0) return null;
        return b.toString("UTF-8");
    }

    private static String line(InputStream in) throws IOException {
        String l = readLine(in);
        return l == null ? "" : l;
    }

    // The button's connection: Jarvis says when a conversation needs the microphone.
    private void serveKeys(InputStream in, OutputStream out) throws IOException {
        Log.i(Hud.TAG, "core button connected");
        keys = out;
        try {
            for (String l; (l = readLine(in)) != null; ) {
                if (l.startsWith("card ")) {
                    Cards c = onCard;
                    if (c != null) c.card(l.substring(5));
                    continue;
                }
                if (l.startsWith("tool ")) {
                    String[] parts = l.split(" ", 3);
                    Tools t = onTool;
                    if (t != null && parts.length == 3) t.run(parts[1], parts[2]);
                    else if (parts.length >= 2) result(parts[1], "Error: no tools in this app");
                    continue;
                }
                l = l.trim();
                if (l.equals("mic 1")) {
                    coreMic = true;
                    awaitUntil = 0;
                    focus(true);
                } else if (l.equals("mic 0")) {
                    coreMic = false;
                    awaitUntil = 0;
                    if (!holding) focus(false);
                    holdTalk = false; // the conversation is over: the next one decides anew
                }
            }
        } finally {
            if (keys == out) keys = null;
            coreMic = false;
        }
    }

    // The microphone: what was heard, else silence, paced at real time by the 20 ms poll.
    private void serveMic(OutputStream out) throws IOException, InterruptedException {
        Log.i(Hud.TAG, "core microphone connected");
        heard.clear();
        byte[] bytes = new byte[CHUNK * 2];
        short[] silence = new short[CHUNK];
        while (running) {
            // The phone with its microphone closed sends nothing: 50 frames of silence a second
            // kept the core busy all day (2 % of a core with the screen off, its wake word model
            // on silence, 01.11.2026). The core's poll slows down meanwhile (audio/mic.go).
            short[] chunk = heard.poll(am != null && !open ? 200 : 20, TimeUnit.MILLISECONDS);
            if (chunk == null && am != null && !open) continue;
            if (chunk == null) chunk = silence;
            for (int i = 0; i < CHUNK; i++) {
                bytes[2 * i] = (byte) chunk[i];
                bytes[2 * i + 1] = (byte) (chunk[i] >> 8);
            }
            out.write(bytes);
        }
    }

    // record reads the microphone: on the TV the remote's (read blocks while the key is not held),
    // on the phone its own while micWanted.
    @SuppressLint("MissingPermission") // the TV: granted by the install script; the phone asks at the start
    private void record(int gen) {
        while (running && gen == generation) {
            if (!micWanted()) {
                sleep(am != null ? 50 : 20); // the phone: fewer wake-ups while nothing listens
                continue;
            }
            AudioRecord rec = null;
            // The phone: the wake word plainly. A conversation through the call path (its echo
            // cancellation, the headset's microphone) only with earbuds, a headset or the car,
            // whose microphone is reached only so; on the phone's own loudspeaker plainly too,
            // Jarvis as media: the call path made him sound like a phone call (01.11.2026), and
            // the core's echo gate keeps him from hearing himself (live/bargein.go). A change of
            // conversation and wake word opens the microphone anew.
            boolean talk = am != null && conversation();
            boolean comm = talk && headset();
            try {
                int min = AudioRecord.getMinBufferSize(MIC_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT);
                int source = comm ? MediaRecorder.AudioSource.VOICE_COMMUNICATION : MediaRecorder.AudioSource.VOICE_RECOGNITION;
                if (comm) route(true);
                callAudio = comm;
                rec = new AudioRecord(source, MIC_RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT,
                        Math.max(min, MIC_RATE));
                rec.startRecording();
                if (comm && car) cue();
                micFrames = 0;
                open = am == null || talk;
                Log.i(Hud.TAG, (am == null ? "remote microphone" : comm ? "phone microphone over the headset"
                        : talk ? "phone microphone" : "phone microphone for the wake word")
                        + " open (state " + rec.getRecordingState() + ")");
                while (running && gen == generation && micWanted() && (am == null || conversation() == talk)) {
                    short[] chunk = new short[CHUNK];
                    int n = rec.read(chunk, 0, CHUNK);
                    if (n < 0) throw new IllegalStateException("read " + n);
                    if (n == 0) continue;
                    if (talk && !comm) {
                        if (aec != null && n == CHUNK) cancel(rec, chunk);
                        else gate(chunk, n);
                        continue;
                    }
                    if (am == null || talk) measure(chunk, n); // the user's voice: the hologram listens with it
                    hear(chunk);
                }
                if (am != null) Log.i(Hud.TAG, "phone microphone closed");
            } catch (Exception e) {
                Log.w(Hud.TAG, (am != null ? "phone" : "remote") + " microphone: " + e + ", again in 2 s");
                sleep(2000);
            } finally {
                open = false;
                callAudio = false;
                if (rec != null) rec.release();
                if (comm) route(false);
            }
        }
    }

    // cue: a short tone in the car once its microphone listens. After "Jarvis" the music pauses and
    // the call path opens; the user waited for a sign and said nothing, and a start without words
    // ends unnoticed (#75, 03.11.2026).
    private void cue() {
        try {
            android.media.ToneGenerator t = new android.media.ToneGenerator(AudioManager.STREAM_VOICE_CALL, 70);
            t.startTone(android.media.ToneGenerator.TONE_PROP_BEEP, 150);
            new Thread(() -> {
                sleep(400);
                t.release();
            }).start();
        } catch (Exception e) {
            Log.w(Hud.TAG, "cue: " + e);
        }
    }

    private void hear(short[] chunk) {
        if (!heard.offer(chunk)) {
            heard.poll();
            heard.offer(chunk);
        }
    }

    // The echo gate on the phone's loudspeaker (no call path, so no echo cancellation): while
    // Jarvis' voice is audible the core gets silence, so he never hears himself. On the
    // loudspeaker his echo reached the microphone as loud as the user's voice (peaks 0.7-1.0,
    // 01.11.2026): no level tells them apart, a detector by level cut him off every two seconds.
    // The user interrupts him with a tap (release), until an echo canceller with his voice as
    // reference lets a voice through.
    private static final long PLAY_WINDOW = 700;    // ms: written audio is audible within this
    private static final long BARGE_QUIET = 1200;   // ms: Jarvis' audio in flight dropped after a barge-in
    private volatile long lastVoice, bargeUntil;
    private volatile boolean flushVoice;
    private final short[] gateSilence = new short[CHUNK];

    private void played(byte[] b, int n) {
        int peak = 0;
        for (int i = 0; i + 1 < n; i += 2) peak = Math.max(peak, Math.abs((short) ((b[i] & 0xff) | (b[i + 1] << 8))));
        if (peak >= 300) lastVoice = System.currentTimeMillis(); // the stream's silence is no voice
    }

    // speaking: Jarvis' voice is audible now.
    private boolean speaking() {
        long now = System.currentTimeMillis();
        return now - lastVoice <= PLAY_WINDOW && now >= bargeUntil;
    }

    private void gate(short[] chunk, int n) {
        if (speaking()) {
            hear(gateSilence);
            return;
        }
        measure(chunk, n);
        hear(chunk);
    }

    // --- The echo canceller on the phone's loudspeaker (EchoCanceller, SpeexDSP) ----------------
    // Jarvis plays as media there (good sound) and the microphone records plainly; his voice, as
    // it is played, is taken off the microphone, so he neither hears himself nor is the user cut
    // off: they can interrupt him by voice, GPT Live hears them over him. The reference must meet
    // the echo in time: the voice is kept in a ring at 16 kHz where Android says it leaves the
    // loudspeaker (AudioTrack.getTimestamp), and each microphone frame takes the reference of the
    // moment it was recorded (AudioRecord.getTimestamp), a little earlier: the canceller's tail
    // (256 ms) covers the echo's path and the timing's slack, but a reference later than its
    // echo cancels nothing. Without the library (the Fire TV) the gate above stands in.
    private final EchoCanceller aec = EchoCanceller.create();
    private static final int REF_LEN = EchoCanceller.RATE * 4;     // 4 s of reference
    // the reference 32 ms earlier than the frame: the echo came 40-44 ms after the reference taken
    // 10 ms early (measured on the S25's loudspeaker, 01.11.2026, scratch aec_lag.py), so it now
    // comes ~10 ms after it and the tail is not spent on the delay
    private static final int REF_LEAD = EchoCanceller.RATE * 32 / 1000;
    private final short[] ref = new short[REF_LEN];
    private final long refBase = System.nanoTime();
    private long refEnd;                                            // the ring's sample index written up to
    private long micFrames;                                         // frames read by this AudioRecord
    private final short[] refFrame = new short[CHUNK], cleaned = new short[CHUNK];
    private final android.media.AudioTimestamp micTs = new android.media.AudioTimestamp();
    private long aecLogAt;
    private double aecMic, aecOut;

    private long refIndex(long nanos) {
        return Math.round((nanos - refBase) / (1e9 / EchoCanceller.RATE));
    }

    // reference: a piece of Jarvis' voice (s16le at rate, mono) that leaves the loudspeaker at nanos.
    private void reference(byte[] b, int n, int rate, long nanos) {
        if (aec == null) return;
        int in = n / 2;
        double step = (double) rate / EchoCanceller.RATE;
        int out = (int) (in / step);
        long start = refIndex(nanos);
        synchronized (ref) {
            if (start > refEnd) { // a pause since the last piece: silence in between
                for (long i = Math.max(refEnd, start - REF_LEN); i < start; i++) ref[(int) (i % REF_LEN)] = 0;
            }
            for (int k = 0; k < out; k++) {
                double p = k * step;
                int i = (int) p;
                double f = p - i;
                int a = (short) ((b[2 * i] & 0xff) | (b[2 * i + 1] << 8));
                int c = i + 1 < in ? (short) ((b[2 * i + 2] & 0xff) | (b[2 * i + 3] << 8)) : a;
                long at = start + k;
                if (at >= 0) ref[(int) (at % REF_LEN)] = (short) (a + (c - a) * f);
            }
            refEnd = Math.max(refEnd, start + out);
        }
    }

    // cancel: one microphone frame with the echo of Jarvis' voice taken off, to the core.
    private void cancel(AudioRecord rec, short[] chunk) {
        micFrames += CHUNK;
        long first = micFrames - CHUNK, nanos;
        if (rec.getTimestamp(micTs, android.media.AudioTimestamp.TIMEBASE_MONOTONIC) == AudioRecord.SUCCESS) {
            nanos = micTs.nanoTime + (long) ((first - micTs.framePosition) * (1e9 / EchoCanceller.RATE));
        } else {
            nanos = System.nanoTime() - (long) (2 * CHUNK * (1e9 / EchoCanceller.RATE));
        }
        long at = refIndex(nanos) - REF_LEAD;
        synchronized (ref) {
            for (int i = 0; i < CHUNK; i++) {
                long k = at + i;
                refFrame[i] = k >= refEnd || k < refEnd - REF_LEN || k < 0 ? 0 : ref[(int) (k % REF_LEN)];
            }
        }
        aec.process(chunk, refFrame, cleaned);
        dump(chunk, refFrame, cleaned);
        short[] out = cleaned.clone();
        measure(out, CHUNK);
        hear(out);
        if (speaking()) { // how much of his voice is left, for tuning (every 5 s while he speaks)
            aecMic += rms(chunk);
            aecOut += rms(out);
            long now = System.currentTimeMillis();
            if (now - aecLogAt > 5000 && aecMic > 0) {
                Log.i(Hud.TAG, String.format(java.util.Locale.ROOT, "echo canceller: %.1f dB taken off his voice",
                        20 * Math.log10(aecMic / Math.max(1, aecOut))));
                aecLogAt = now;
                aecMic = aecOut = 0;
            }
        }
    }

    // dump: for tuning the timing, while the file aecDump names exists (run/aec-dump, made by hand
    // over ADB): the microphone, the reference and the result of 30 s of a conversation, 16 kHz
    // s16le, one file each next to it; the flag file is removed after.
    public static volatile java.io.File aecDump;
    private java.io.OutputStream dumpMic, dumpRef, dumpOut;
    private int dumpFrames;

    private void dump(short[] mic, short[] r, short[] out) {
        java.io.File flag = aecDump;
        if (flag == null) return;
        try {
            if (dumpMic == null) {
                if (!flag.exists()) return;
                java.io.File dir = flag.getParentFile();
                dumpMic = new java.io.FileOutputStream(new java.io.File(dir, "aec-mic.raw"));
                dumpRef = new java.io.FileOutputStream(new java.io.File(dir, "aec-ref.raw"));
                dumpOut = new java.io.FileOutputStream(new java.io.File(dir, "aec-out.raw"));
                dumpFrames = 0;
                Log.i(Hud.TAG, "echo canceller: recording 30 s for tuning");
            }
            for (Object[] p : new Object[][]{{dumpMic, mic}, {dumpRef, r}, {dumpOut, out}}) {
                short[] s = (short[]) p[1];
                byte[] b = new byte[s.length * 2];
                for (int i = 0; i < s.length; i++) {
                    b[2 * i] = (byte) s[i];
                    b[2 * i + 1] = (byte) (s[i] >> 8);
                }
                ((java.io.OutputStream) p[0]).write(b);
            }
            if (++dumpFrames >= 1500) { // 30 s
                dumpMic.close();
                dumpRef.close();
                dumpOut.close();
                dumpMic = null;
                flag.delete();
                Log.i(Hud.TAG, "echo canceller: recording done");
            }
        } catch (Exception e) {
            Log.w(Hud.TAG, "echo canceller dump: " + e);
            aecDump = null;
        }
    }

    private static double rms(short[] s) {
        double sum = 0;
        for (short v : s) sum += (double) v * v;
        return Math.sqrt(sum / s.length);
    }

    // barge: the user interrupts Jarvis (a tap while he speaks): he stops, the app's queued voice
    // goes, and the core drops his audio still in flight (live/bargein.go).
    private void barge() {
        Log.i(Hud.TAG, "barge-in by the button");
        bargeUntil = System.currentTimeMillis() + BARGE_QUIET;
        flushVoice = true;
        send("barge");
    }

    // callAudio: the conversation runs over the call path (a headset): Jarvis' voice goes there too.
    private volatile boolean callAudio;

    // headset: earbuds, a headset or the car to talk over.
    private boolean headset() {
        if (Build.VERSION.SDK_INT >= 31) {
            for (AudioDeviceInfo d : am.getAvailableCommunicationDevices()) {
                switch (d.getType()) {
                    case AudioDeviceInfo.TYPE_BLUETOOTH_SCO:
                    case AudioDeviceInfo.TYPE_BLE_HEADSET:
                    case AudioDeviceInfo.TYPE_WIRED_HEADSET:
                    case AudioDeviceInfo.TYPE_USB_HEADSET:
                        return true;
                    default:
                        break;
                }
            }
            return false;
        }
        for (AudioDeviceInfo d : am.getDevices(AudioManager.GET_DEVICES_INPUTS)) {
            switch (d.getType()) {
                case AudioDeviceInfo.TYPE_BLUETOOTH_SCO:
                case AudioDeviceInfo.TYPE_WIRED_HEADSET:
                case AudioDeviceInfo.TYPE_USB_HEADSET:
                    return true;
                default:
                    break;
            }
        }
        return false;
    }

    private static void sleep(long ms) {
        try {
            Thread.sleep(ms);
        } catch (InterruptedException ignored) {
        }
    }

    // route: the phone in communication mode while it listens, so its echo cancellation hears what
    // Jarvis says; he speaks on the communication device then (earbuds, the car, a headset, else
    // the loudspeaker instead of the earpiece).
    private void route(boolean on) {
        try {
            if (!on) {
                if (Build.VERSION.SDK_INT >= 31) am.clearCommunicationDevice();
                else am.setSpeakerphoneOn(false);
                am.setMode(AudioManager.MODE_NORMAL);
                return;
            }
            am.setMode(AudioManager.MODE_IN_COMMUNICATION);
            if (Build.VERSION.SDK_INT < 31) {
                am.setSpeakerphoneOn(!am.isBluetoothScoOn() && !am.isWiredHeadsetOn());
                return;
            }
            AudioDeviceInfo speaker = null, other = null;
            for (AudioDeviceInfo d : am.getAvailableCommunicationDevices()) {
                switch (d.getType()) {
                    case AudioDeviceInfo.TYPE_BLUETOOTH_SCO:
                    case AudioDeviceInfo.TYPE_BLE_HEADSET:
                    case AudioDeviceInfo.TYPE_WIRED_HEADSET:
                    case AudioDeviceInfo.TYPE_USB_HEADSET:
                        if (other == null) other = d;
                        break;
                    case AudioDeviceInfo.TYPE_BUILTIN_SPEAKER:
                        speaker = d;
                        break;
                    default:
                        break;
                }
            }
            AudioDeviceInfo use = other != null ? other : speaker;
            if (use != null) am.setCommunicationDevice(use);
        } catch (Exception e) {
            Log.w(Hud.TAG, "audio route: " + e);
        }
    }

    private static AudioTrack track(int rate, int mask, int min, int channels, boolean music, boolean call) {
        AudioTrack track = new AudioTrack.Builder()
                .setAudioAttributes(new AudioAttributes.Builder()
                        .setUsage(music ? AudioAttributes.USAGE_MEDIA
                                : call ? AudioAttributes.USAGE_VOICE_COMMUNICATION : AudioAttributes.USAGE_ASSISTANT)
                        .setContentType(music ? AudioAttributes.CONTENT_TYPE_MUSIC : AudioAttributes.CONTENT_TYPE_SPEECH)
                        .build())
                .setAudioFormat(new AudioFormat.Builder().setSampleRate(rate).setChannelMask(mask)
                        .setEncoding(AudioFormat.ENCODING_PCM_16BIT).build())
                .setBufferSizeInBytes(Math.max(min * 2, rate * channels / 2)) // ~250 ms
                .setTransferMode(AudioTrack.MODE_STREAM)
                .build();
        track.play();
        return track;
    }

    private void play(InputStream in, int rate, int channels, String name) throws IOException {
        int mask = channels == 2 ? AudioFormat.CHANNEL_OUT_STEREO : AudioFormat.CHANNEL_OUT_MONO;
        int min = AudioTrack.getMinBufferSize(rate, mask, AudioFormat.ENCODING_PCM_16BIT);
        boolean music = name.contains("music");
        // The phone in a conversation over a headset: speech on its call path, which the echo
        // cancellation takes as its reference. The core keeps its voice stream open from its
        // start, so the track follows the call path, rebuilt when it begins and ends.
        boolean call = am != null && !music && callAudio;
        AudioTrack track = track(rate, mask, min, channels, music, call);
        long written = 0; // frames written to this track (the echo canceller's timing)
        android.media.AudioTimestamp ts = new android.media.AudioTimestamp();
        byte[] buf = new byte[4096];
        try {
            for (int n; running && (n = in.read(buf)) > 0; ) {
                boolean now = am != null && !music && callAudio;
                if (now != call) {
                    call = now;
                    track.pause();
                    track.flush();
                    track.release();
                    track = track(rate, mask, min, channels, music, call);
                    written = 0;
                    Log.i(Hud.TAG, "voice as " + (call ? "a call (echo cancelled)" : "the assistant"));
                }
                if (!music && am != null && System.currentTimeMillis() < bargeUntil) {
                    // interrupted: what is queued in the track goes too, and his audio in flight
                    if (flushVoice) {
                        flushVoice = false;
                        track.pause();
                        track.flush();
                        track.play();
                    }
                    continue;
                }
                long before = written;
                track.write(buf, 0, n & ~1);
                written += (n & ~1) / 2 / channels;
                measure(buf, n & ~1); // Jarvis' voice: the equalizer pulse
                if (!music && am != null) {
                    played(buf, n & ~1);
                    if (!call && channels == 1) { // the echo canceller's reference: when it leaves the loudspeaker
                        long at;
                        if (track.getTimestamp(ts)) {
                            at = ts.nanoTime + (long) ((before - ts.framePosition) * (1e9 / rate));
                        } else {
                            at = System.nanoTime() + (long) ((before - written + track.getBufferSizeInFrames()) * (1e9 / rate));
                        }
                        reference(buf, n & ~1, rate, at);
                    }
                }
            }
        } finally {
            track.pause();
            track.flush();
            track.release();
        }
    }
}
