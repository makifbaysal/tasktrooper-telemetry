package com.tasktrooper.gradle

import com.sun.net.httpserver.HttpServer
import java.net.InetSocketAddress
import java.util.concurrent.CopyOnWriteArrayList

/** Minimal stand-in for services/symbolicator `POST /v1/artifacts`. */
class FakeSymbolicator(private val status: Int = 201) : AutoCloseable {
    data class Request(val method: String, val path: String, val headers: Map<String, String>, val body: ByteArray) {
        val contentType: String get() = headers["content-type"] ?: ""

        /** name → (filename, value) for each multipart part. */
        fun parts(): Map<String, Pair<String?, String>> {
            val boundary = contentType.substringAfter("boundary=")
            val text = body.toString(Charsets.ISO_8859_1)
            return text.split("--$boundary").drop(1).filter { !it.startsWith("--") }.associate { raw ->
                val part = raw.removePrefix("\r\n").removeSuffix("\r\n")
                val (head, value) = part.split("\r\n\r\n", limit = 2)
                val name = Regex("name=\"([^\"]+)\"").find(head)!!.groupValues[1]
                val filename = Regex("filename=\"([^\"]*)\"").find(head)?.groupValues?.get(1)
                name to (filename to String(value.toByteArray(Charsets.ISO_8859_1), Charsets.UTF_8))
            }
        }
    }

    val requests = CopyOnWriteArrayList<Request>()
    private val server = HttpServer.create(InetSocketAddress("127.0.0.1", 0), 0).apply {
        createContext("/") { ex ->
            val body = ex.requestBody.readBytes()
            requests += Request(ex.requestMethod, ex.requestURI.path,
                ex.requestHeaders.entries.associate { it.key.lowercase() to it.value.first() }, body)
            val resp = """{"ok":true}""".toByteArray()
            ex.sendResponseHeaders(status, resp.size.toLong())
            ex.responseBody.use { it.write(resp) }
        }
        start()
    }
    val url: String get() = "http://127.0.0.1:${server.address.port}"
    override fun close() = server.stop(0)
}
