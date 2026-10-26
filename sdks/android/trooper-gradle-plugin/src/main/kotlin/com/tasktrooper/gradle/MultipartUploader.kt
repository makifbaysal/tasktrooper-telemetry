package com.tasktrooper.gradle

import java.io.ByteArrayOutputStream
import java.io.File
import java.net.HttpURLConnection
import java.net.URI
import java.util.UUID

/** Result of an artifact upload: HTTP status (or -1 on I/O error) and response body / error message. */
data class UploadResult(val status: Int, val body: String) {
    val ok: Boolean get() = status in 200..299
}

/**
 * `POST <apiUrl>/v1/artifacts` (contracts.md §12, same contract as `trooper-cli upload-mapping`):
 * `Authorization: Bearer <key>`, multipart/form-data with `kind`, `app`, `release` and the file part `file`.
 * Plain [HttpURLConnection], no dependencies.
 */
object MultipartUploader {
    fun upload(
        apiUrl: String,
        apiKey: String,
        kind: String,
        app: String,
        release: String,
        file: File,
        fileName: String = file.name,
        timeoutMs: Int = 60_000,
    ): UploadResult {
        val boundary = "----trooper" + UUID.randomUUID().toString().replace("-", "")
        val url = URI(apiUrl.trimEnd('/') + "/v1/artifacts").toURL()
        return try {
            val conn = url.openConnection() as HttpURLConnection
            conn.requestMethod = "POST"
            conn.doOutput = true
            conn.connectTimeout = timeoutMs
            conn.readTimeout = timeoutMs
            conn.setRequestProperty("Authorization", "Bearer $apiKey")
            conn.setRequestProperty("Content-Type", "multipart/form-data; boundary=$boundary")
            conn.setRequestProperty("User-Agent", "trooper-gradle-plugin")
            val head = ByteArrayOutputStream()
            fun text(s: String) = head.write(s.toByteArray(Charsets.UTF_8))
            for ((k, v) in listOf("kind" to kind, "app" to app, "release" to release)) {
                text("--$boundary\r\nContent-Disposition: form-data; name=\"$k\"\r\n\r\n$v\r\n")
            }
            text("--$boundary\r\nContent-Disposition: form-data; name=\"file\"; filename=\"${fileName.replace("\"", "")}\"\r\n")
            text("Content-Type: application/octet-stream\r\n\r\n")
            val tail = "\r\n--$boundary--\r\n".toByteArray(Charsets.UTF_8)
            conn.setFixedLengthStreamingMode(head.size().toLong() + file.length() + tail.size)
            conn.outputStream.use { out ->
                head.writeTo(out)
                file.inputStream().use { it.copyTo(out) }
                out.write(tail)
            }
            val status = conn.responseCode
            val stream = if (status >= 400) conn.errorStream else conn.inputStream
            val body = stream?.use { it.readBytes().toString(Charsets.UTF_8) } ?: ""
            conn.disconnect()
            UploadResult(status, body.take(2_000))
        } catch (e: Exception) {
            UploadResult(-1, e.toString())
        }
    }
}
