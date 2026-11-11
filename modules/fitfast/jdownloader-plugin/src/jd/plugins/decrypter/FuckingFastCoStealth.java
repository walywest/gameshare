// PROTOTYPE — not compiled or run against a real JDownloader build from this
// environment (no JDownloader jar / plugin API available here to compile
// against). Modeled on JDownloader's own PluginForDecrypt shape (see e.g.
// src/jd/plugins/decrypter/BergFilesCom.java in the JDownloader source) but
// the exact base-class/annotation surface can drift between JD releases —
// expect to adjust imports/signatures to match the JDownloader version you
// actually install this into. See ../README.md for the full picture, install
// steps, and what to check first if it doesn't compile as-is.
//
// What this plugin does, and why it's shaped this way:
//
//   fuckingfast.co sits behind Cloudflare's bot check, which JDownloader's
//   plain HTTP engine cannot pass (no way to run the JS challenge, and its
//   TLS/HTTP fingerprint doesn't read as a real browser). This project's
//   Python side already solves that with Camoufox (a source-patched real
//   Firefox) driving the click-through to a pre-signed, cookie-free
//   dl.fuckingfast.co URL — see app/resolver.py.
//
//   Establishing that trust is a per-session cost, not a per-link one, so
//   rather than shelling out to Python fresh for every one of a repack's
//   ~150-250 part links, this plugin talks to resolver_daemon.py — a small
//   local HTTP service that keeps ONE browser alive and resolves links
//   against it as they come in. Start the daemon once; JDownloader just does
//   a localhost HTTP GET per link and hands the returned signed URL to its
//   own (perfectly good) download engine, which gets pause/resume, the
//   queue, and the GUI it already has for free.
//
// Requires: `python resolver_daemon.py` running locally (default
// http://127.0.0.1:8765) before you start grabbing fuckingfast.co links.

package jd.plugins.decrypter;

import java.io.BufferedReader;
import java.io.File;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URI;
import java.net.URL;
import java.net.URLEncoder;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.regex.Matcher;
import java.util.regex.Pattern;

import jd.PluginWrapper;
import jd.controlling.ProgressController;
import jd.plugins.CryptedLink;
import jd.plugins.DecrypterException;
import jd.plugins.DecrypterPlugin;
import jd.plugins.DownloadLink;
import jd.plugins.PluginForDecrypt;

@DecrypterPlugin(
    revision = "$Revision$",
    interfaceVersion = 2,
    names = { "fuckingfast.co" },
    urls = { "https?://(www\\.)?fuckingfast\\.co/[A-Za-z0-9]+(#.*)?" },
    flags = { 0 }
)
public class FuckingFastCoStealth extends PluginForDecrypt {

    // Change this if you run resolver_daemon.py with --port on something else.
    private static final String DAEMON_BASE = "http://127.0.0.1:8765";
    private static final int CONNECT_TIMEOUT_MS = 5_000;
    // A resolve is a real page load + two button clicks in a real browser;
    // give it real headroom rather than JDownloader's usual short timeouts.
    private static final int READ_TIMEOUT_MS = 45_000;

    // Absolute path to the repo checkout this plugin was copied out of, so
    // decryptIt() can lazily spawn resolver_daemon.py if it isn't already
    // running instead of forcing the user to remember to start it by hand.
    // EDIT THIS to match where you cloned FitFast-Downloader.
    private static final String REPO_ROOT = "/home/albus/FitFast-Downloader";
    private static final String DAEMON_SCRIPT = REPO_ROOT + "/jdownloader-plugin/resolver_daemon.py";
    private static final String PYTHON_BIN = "python3";

    // How long to wait for a freshly-spawned daemon to report /health before
    // giving up (its first request loads Camoufox + a full browser profile,
    // which is much slower than a normal HTTP round trip).
    private static final int DAEMON_STARTUP_TIMEOUT_MS = 30_000;
    private static final int HEALTH_CHECK_TIMEOUT_MS = 1_500;

    // Shared across every instance of this plugin within one JDownloader
    // process: only ever spawn one daemon, never one per decryptIt() call.
    private static final Object DAEMON_LOCK = new Object();
    private static Process daemonProcess;

    private static final Pattern SIGNED_URL_RE =
        Pattern.compile("\"signed_url\"\\s*:\\s*\"((?:\\\\.|[^\"\\\\])*)\"");
    private static final Pattern FILENAME_RE =
        Pattern.compile("\"filename\"\\s*:\\s*\"((?:\\\\.|[^\"\\\\])*)\"");
    private static final Pattern ERROR_RE =
        Pattern.compile("\"error\"\\s*:\\s*\"((?:\\\\.|[^\"\\\\])*)\"");

    public FuckingFastCoStealth(PluginWrapper wrapper) {
        super(wrapper);
    }

    @Override
    public ArrayList<DownloadLink> decryptIt(CryptedLink param, ProgressController progress) throws Exception {
        ArrayList<DownloadLink> decryptedLinks = new ArrayList<DownloadLink>();
        final String originalUrl = param.toString();

        ensureDaemonRunning();

        final String encoded = URLEncoder.encode(originalUrl, StandardCharsets.UTF_8.name());
        final URL resolveUrl = new URI(DAEMON_BASE + "/resolve?url=" + encoded).toURL();

        String body;
        int status;
        try {
            HttpURLConnection conn = (HttpURLConnection) resolveUrl.openConnection();
            conn.setConnectTimeout(CONNECT_TIMEOUT_MS);
            conn.setReadTimeout(READ_TIMEOUT_MS);
            conn.setRequestMethod("GET");
            status = conn.getResponseCode();
            java.io.InputStream stream = (status == 200) ? conn.getInputStream() : conn.getErrorStream();
            StringBuilder sb = new StringBuilder();
            try (BufferedReader r = new BufferedReader(new InputStreamReader(stream, StandardCharsets.UTF_8))) {
                String line;
                while ((line = r.readLine()) != null) sb.append(line);
            }
            body = sb.toString();
        } catch (java.net.ConnectException ce) {
            // Most likely cause: resolver_daemon.py isn't running and the
            // auto-launch above also failed (see ensureDaemonRunning()).
            logger.severe("Could not reach " + DAEMON_BASE + " — is resolver_daemon.py running? "
                + "(python jdownloader-plugin/resolver_daemon.py)");
            throw new DecrypterException("FuckingFast resolver daemon not reachable at "
                + DAEMON_BASE + ". Start it with: python resolver_daemon.py");
        }

        if (status != 200) {
            final Matcher em = ERROR_RE.matcher(body);
            final String msg = em.find() ? jsonUnescape(em.group(1)) : ("HTTP " + status);
            logger.warning("Resolve failed for " + originalUrl + ": " + msg);
            // Cloudflare hiccup / captcha / dead link — surface it, don't crash the crawl.
            throw new DecrypterException("Resolve failed: " + msg);
        }

        final Matcher um = SIGNED_URL_RE.matcher(body);
        final Matcher fm = FILENAME_RE.matcher(body);
        if (!um.find() || !fm.find()) {
            logger.warning("Unexpected resolver response for " + originalUrl + ": " + body);
            return null; // JD convention: null means "decrypter is broken", not "0 links"
        }
        final String signedUrl = jsonUnescape(um.group(1));
        final String filename = jsonUnescape(fm.group(1));

        DownloadLink dl = createDownloadlink(signedUrl);
        dl.setFinalFileName(filename);
        // The signed URL is Range-capable and cookie-free (see app/resolver.py's
        // header comment) — plain direct HTTP is all JDownloader needs from here.
        decryptedLinks.add(dl);
        return decryptedLinks;
    }

    /**
     * Best-effort: if the daemon is already up, this is one fast /health hit
     * and nothing else happens. If it isn't, spawn it once (guarded so a
     * flood of links doesn't spawn a flood of daemons) and poll /health
     * until it answers or DAEMON_STARTUP_TIMEOUT_MS runs out. Any failure
     * here is swallowed — the real HTTP call right after this is what
     * ultimately reports success/failure to JDownloader.
     */
    private void ensureDaemonRunning() {
        if (isDaemonHealthy()) return;
        synchronized (DAEMON_LOCK) {
            if (isDaemonHealthy()) return;
            if (daemonProcess != null && daemonProcess.isAlive()) return;
            try {
                File script = new File(DAEMON_SCRIPT);
                if (!script.isFile()) {
                    logger.warning("resolver_daemon.py not found at " + DAEMON_SCRIPT
                        + " — update REPO_ROOT in FuckingFastCoStealth.java, or start the daemon manually.");
                    return;
                }
                logger.info("Starting resolver_daemon.py (" + DAEMON_SCRIPT + ")...");
                ProcessBuilder pb = new ProcessBuilder(PYTHON_BIN, DAEMON_SCRIPT);
                pb.directory(new File(REPO_ROOT));
                pb.redirectErrorStream(true);
                pb.redirectOutput(new File(REPO_ROOT, "jdownloader-plugin/resolver_daemon.log"));
                daemonProcess = pb.start();
                Runtime.getRuntime().addShutdownHook(new Thread(() -> {
                    if (daemonProcess != null && daemonProcess.isAlive()) daemonProcess.destroy();
                }));
            } catch (Exception e) {
                logger.severe("Failed to auto-launch resolver_daemon.py: " + e);
                return;
            }
        }
        final long deadline = System.currentTimeMillis() + DAEMON_STARTUP_TIMEOUT_MS;
        while (System.currentTimeMillis() < deadline) {
            if (isDaemonHealthy()) return;
            try { Thread.sleep(500); } catch (InterruptedException ie) { Thread.currentThread().interrupt(); return; }
        }
        logger.warning("resolver_daemon.py did not become healthy within "
            + DAEMON_STARTUP_TIMEOUT_MS + "ms of launch — Camoufox/browser startup may still be in progress.");
    }

    private boolean isDaemonHealthy() {
        try {
            HttpURLConnection conn = (HttpURLConnection) new URI(DAEMON_BASE + "/health").toURL().openConnection();
            conn.setConnectTimeout(HEALTH_CHECK_TIMEOUT_MS);
            conn.setReadTimeout(HEALTH_CHECK_TIMEOUT_MS);
            conn.setRequestMethod("GET");
            return conn.getResponseCode() == 200;
        } catch (Exception e) {
            return false;
        }
    }

    /** Undoes the subset of JSON string escaping resolver_daemon.py's json.dumps can produce. */
    private static String jsonUnescape(String s) {
        StringBuilder out = new StringBuilder(s.length());
        for (int i = 0; i < s.length(); i++) {
            char c = s.charAt(i);
            if (c != '\\' || i + 1 >= s.length()) { out.append(c); continue; }
            char next = s.charAt(++i);
            switch (next) {
                case '"': out.append('"'); break;
                case '\\': out.append('\\'); break;
                case '/': out.append('/'); break;
                case 'n': out.append('\n'); break;
                case 'r': out.append('\r'); break;
                case 't': out.append('\t'); break;
                case 'b': out.append('\b'); break;
                case 'f': out.append('\f'); break;
                case 'u':
                    if (i + 4 < s.length()) {
                        out.append((char) Integer.parseInt(s.substring(i + 1, i + 5), 16));
                        i += 4;
                    }
                    break;
                default: out.append(next);
            }
        }
        return out.toString();
    }
}
