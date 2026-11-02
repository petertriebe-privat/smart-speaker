package dev.jarvis.shared;

import android.content.Context;
import android.util.Log;

import java.io.BufferedReader;
import java.io.File;
import java.io.FileWriter;
import java.io.InputStreamReader;
import java.util.List;
import java.util.Map;

// Runs the Jarvis core (core/, built for ARMv7 into the APK's native libraries as libjarvis.so:
// Android executes only from there) as a child process, and starts it again when it ends, as
// the Echo's service loop does. Its log goes to logcat (tag JarvisCore) and to run/jarvis.log.
public final class CoreProcess {
    public static final String TAG = "JarvisCore";
    private static final long LOG_LIMIT = 4 << 20;

    private final Context ctx;
    private final List<String> args;
    private final Map<String, String> env;
    private volatile boolean running;
    private volatile Process process;

    public CoreProcess(Context ctx, List<String> args, Map<String, String> env) {
        this.ctx = ctx;
        this.args = args;
        this.env = env;
    }

    public static File binary(Context ctx) {
        return new File(ctx.getApplicationInfo().nativeLibraryDir, "libjarvis.so");
    }

    public void start() {
        running = true;
        Thread t = new Thread(this::loop, "jarvis-core");
        t.setDaemon(true);
        t.start();
    }

    public void stop() {
        running = false;
        Process p = process;
        if (p != null) p.destroy();
    }

    private void loop() {
        File run = new File(ctx.getFilesDir(), "run");
        run.mkdirs();
        File log = new File(run, "jarvis.log");
        while (running) {
            try {
                if (log.length() > LOG_LIMIT) log.renameTo(new File(run, "jarvis.log.1"));
                ProcessBuilder pb = new ProcessBuilder(args).directory(ctx.getFilesDir()).redirectErrorStream(true);
                pb.environment().putAll(env);
                process = pb.start();
                Log.i(TAG, "started " + args.get(0));
                try (BufferedReader r = new BufferedReader(new InputStreamReader(process.getInputStream()));
                     FileWriter w = new FileWriter(log, true)) {
                    for (String line; (line = r.readLine()) != null; ) {
                        Log.i(TAG, line);
                        w.write(line);
                        w.write('\n');
                        w.flush();
                    }
                }
                Log.w(TAG, "ended with " + process.waitFor());
            } catch (Exception e) {
                Log.e(TAG, "core: " + e);
            }
            if (!running) break;
            try {
                Thread.sleep(5000); // as the Echo's service loop
            } catch (InterruptedException ignored) {
            }
        }
    }
}
