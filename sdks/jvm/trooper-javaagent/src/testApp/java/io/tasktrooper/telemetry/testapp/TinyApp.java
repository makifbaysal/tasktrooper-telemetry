package io.tasktrooper.telemetry.testapp;

import com.sun.net.httpserver.HttpServer;
import java.net.InetSocketAddress;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;

/** Tiny app run under the agent by the integration test: one HTTP server, one client call to itself. */
public final class TinyApp {
    public static void main(String[] args) throws Exception {
        HttpServer server = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/hello", ex -> {
            byte[] body = "hi".getBytes(StandardCharsets.UTF_8);
            ex.sendResponseHeaders(200, body.length);
            ex.getResponseBody().write(body);
            ex.close();
        });
        server.start();
        HttpClient client = HttpClient.newHttpClient();
        HttpResponse<String> res = client.send(
                HttpRequest.newBuilder(URI.create("http://127.0.0.1:" + server.getAddress().getPort() + "/hello")).build(),
                HttpResponse.BodyHandlers.ofString());
        System.out.println("status=" + res.statusCode());
        Thread.sleep(500);
        server.stop(0);
        System.exit(0); // agent flushes on shutdown
    }
}
