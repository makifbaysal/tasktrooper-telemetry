package com.tasktrooper.gradle

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Rule
import org.junit.Test
import org.junit.rules.TemporaryFolder

class MultipartUploaderTest {
    @get:Rule val tmp = TemporaryFolder()

    @Test fun postsMultipartWithBearerAuth() = FakeSymbolicator().use { server ->
        val mapping = tmp.newFile("m.txt").apply { writeText("com.example.Foo -> a.a:\n    void bar() -> b\n") }
        val r = MultipartUploader.upload(server.url + "/", "tt_key", "r8", "courier-app", "1.4.0+812", mapping, "mapping.txt")
        assertTrue(r.ok)
        val req = server.requests.single()
        assertEquals("POST", req.method)
        assertEquals("/v1/artifacts", req.path)
        assertEquals("Bearer tt_key", req.headers["authorization"])
        assertTrue(req.contentType.startsWith("multipart/form-data; boundary="))
        val parts = req.parts()
        assertEquals("r8", parts["kind"]!!.second)
        assertEquals("courier-app", parts["app"]!!.second)
        assertEquals("1.4.0+812", parts["release"]!!.second)
        assertEquals("mapping.txt", parts["file"]!!.first)
        assertEquals(mapping.readText(), parts["file"]!!.second)
    }

    @Test fun reportsHttpErrors() = FakeSymbolicator(status = 401).use { server ->
        val r = MultipartUploader.upload(server.url, "bad", "r8", "a", "1+1", tmp.newFile("m.txt"))
        assertFalse(r.ok)
        assertEquals(401, r.status)
    }

    @Test fun networkErrorsDoNotThrow() {
        val r = MultipartUploader.upload("http://127.0.0.1:1", "k", "r8", "a", "1+1", tmp.newFile("m.txt"), timeoutMs = 2_000)
        assertEquals(-1, r.status)
    }
}
