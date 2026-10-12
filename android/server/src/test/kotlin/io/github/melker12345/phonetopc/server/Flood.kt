package io.github.melker12345.phonetopc.server

import java.io.Closeable
import java.io.IOException
import java.net.Socket
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import kotlin.concurrent.thread

/**
 * A hostile client on an already connected [socket]: upgrades to a WebSocket, sends [first]
 * and then [request] over and over without ever reading.
 */
class Flood(private val socket: Socket, port: Int, first: String?, request: String) : Closeable {
    private val dropped = CountDownLatch(1)

    init {
        val out = socket.getOutputStream()
        out.write(
            ("GET $WS_PATH HTTP/1.1\r\nHost: 127.0.0.1:$port\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
                "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n").toByteArray(),
        )
        out.flush()
        val input = socket.getInputStream()
        val head = StringBuilder()
        while (!head.endsWith("\r\n\r\n")) head.append(input.read().toChar())
        check(head.startsWith("HTTP/1.1 101")) { head }
        first?.let { out.write(frame(it)) }
        val chunk = frame(request).let { f -> ByteArray(f.size * 64) { f[it % f.size] } }
        thread(isDaemon = true) {
            try {
                while (true) {
                    out.write(chunk)
                    out.flush()
                }
            } catch (_: IOException) {
                dropped.countDown()
            }
        }
    }

    /** Whether the server dropped the connection (our writes failed) within [millis]. */
    fun awaitDropped(millis: Long): Boolean = dropped.await(millis, TimeUnit.MILLISECONDS)

    override fun close() {
        runCatching { socket.close() }
    }

    companion object {
        const val HELLO = """{"jsonrpc":"2.0","id":1,"method":"hello","params":{"protocol":"phonetopc/1"}}"""
        const val LIST_SENSORS = """{"jsonrpc":"2.0","id":2,"method":"list_sensors"}"""

        /** A masked text frame; the all-zero key leaves the payload as is. */
        fun frame(text: String): ByteArray {
            val payload = text.toByteArray()
            require(payload.size <= 0xffff)
            val length = if (payload.size < 126) {
                byteArrayOf((0x80 or payload.size).toByte())
            } else {
                byteArrayOf((0x80 or 126).toByte(), (payload.size shr 8).toByte(), payload.size.toByte())
            }
            return byteArrayOf(0x81.toByte()) + length + byteArrayOf(0, 0, 0, 0) + payload
        }
    }
}
