package io.tasktrooper.telemetry.testapp;

import com.sun.net.httpserver.HttpServer;
import io.opentelemetry.api.trace.Span;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;

/** Run under the agent with TROOPER_PROFILING=true: a CPU-heavy endpoint called in a loop for {@code args[0]} seconds. */
public final class ProfiledApp {
    public static void main(String[] args) throws Exception {
        long seconds = args.length > 0 ? Long.parseLong(args[0]) : 15;
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/burn", ex -> {
            Span.current().setAttribute("trooper.route", "/burn");
            double x = Burner.burn(50);
            byte[] body = Double.toString(x).getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(200, body.length);
            ex.getResponseBody().write(body);
            ex.close();
        });
        server.start();
        HttpClient client = HttpClient.newHttpClient();
        URI uri = URI.create("http://127.0.0.1:" + server.getAddress().getPort() + "/burn");
        long until = System.currentTimeMillis() + seconds * 1000;
        int ok = 0;
        while (System.currentTimeMillis() < until) {
            HttpResponse<String> res = client.send(HttpRequest.newBuilder(uri).build(), HttpResponse.BodyHandlers.ofString());
            if (res.statusCode() == 200) ok++;
        }
        System.out.println("requests=" + ok);
        server.stop(0);
        System.exit(0);
    }

    static final class Burner {
        static double burn(long millis) {
            long end = System.nanoTime() + millis * 1_000_000;
            double r = 0;
            while (System.nanoTime() < end) {
                for (int i = 0; i < 1000; i++) r = Math.sin(r + i);
            }
            return r;
        }
    }
}
