package com.tasktrooper.android.internal

import java.io.ByteArrayOutputStream
import java.io.File
import java.io.FileOutputStream
import java.util.zip.GZIPOutputStream

/**
 * Offline queue: one gzipped `RumBatch` JSON per file, written once (tmp + rename), never modified.
 * Total size capped at [maxBytes]; oldest files are dropped first (FIFO), per contracts.md §9.
 */
internal class DiskQueue(private val dir: File, private val maxBytes: Long) {
    private var seq = 0L

    init {
        Internal.guard("queue.init", Unit) {
            dir.mkdirs()
            dir.listFiles()?.filter { it.name.endsWith(".tmp") }?.forEach { it.delete() }
        }
    }

    /** Writes a batch synchronously. Safe to call from a crashing thread. */
    @Synchronized
    fun write(json: String): Boolean = Internal.guard("queue.write", false) {
        val bytes = gzip(json.toByteArray(Charsets.UTF_8))
        val name = String.format(java.util.Locale.ROOT, "%019d-%06d-%08x", System.currentTimeMillis(), (seq++) % 1_000_000, RANDOM.nextInt())
        val tmp = File(dir, "$name.tmp")
        FileOutputStream(tmp).use { out ->
            out.write(bytes)
            try { out.fd.sync() } catch (_: Throwable) {}
        }
        val ok = tmp.renameTo(File(dir, "$name.batch"))
        trim()
        ok
    }

    @Synchronized
    fun oldest(): File? = files().firstOrNull()

    @Synchronized
    fun remove(file: File) { file.delete() }

    @Synchronized
    fun size(): Int = files().size

    @Synchronized
    fun totalBytes(): Long = files().sumOf { it.length() }

    @Synchronized
    fun all(): List<File> = files()

    private fun files(): List<File> =
        (dir.listFiles { f -> f.name.endsWith(".batch") } ?: emptyArray()).sortedBy { it.name }

    private fun trim() {
        val fs = files().toMutableList()
        var total = fs.sumOf { it.length() }
        while (total > maxBytes && fs.isNotEmpty()) {
            val f = fs.removeAt(0)
            total -= f.length()
            f.delete()
            Internal.droppedEvents.incrementAndGet()
        }
    }

    companion object {
        private val RANDOM = java.util.Random()
        fun gzip(data: ByteArray): ByteArray {
            val bos = ByteArrayOutputStream(data.size / 4 + 64)
            GZIPOutputStream(bos).use { it.write(data) }
            return bos.toByteArray()
        }
    }
}
