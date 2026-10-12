package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import kotlinx.coroutines.runBlocking
import kotlinx.serialization.json.JsonObject
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.fail
import org.junit.Test
import java.io.File
import java.io.IOException
import java.net.InetAddress
import java.net.InetSocketAddress
import java.security.KeyStore
import java.security.MessageDigest
import java.util.Base64
import javax.net.ssl.KeyManagerFactory
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLServerSocket
import kotlin.concurrent.thread
import kotlin.time.Duration.Companion.seconds

/** [PinnedWebSocket] against a hostile PC that sends malformed frames after the upgrade. */
class PinnedWebSocketTest {
    private val vectors = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance", "vectors")
    private val cert = Pins.parsePemCertificate(File(vectors, "pc-cert.pem").readText())
    private val key = Pem.ecPrivateKey(File(vectors, "pc-key.pem").readText())
    private var server: SSLServerSocket? = null

    @After fun tearDown() {
        server?.close()
    }

    /** Accepts one connection, upgrades it and then sends [frame] as raw bytes. */
    private fun hostilePc(frame: ByteArray): Int {
        val store = KeyStore.getInstance("PKCS12").apply {
            load(null, null)
            setKeyEntry("pc", key, CharArray(0), arrayOf(cert))
        }
        val kmf = KeyManagerFactory.getInstance(KeyManagerFactory.getDefaultAlgorithm()).apply { init(store, CharArray(0)) }
        val context = SSLContext.getInstance("TLS").apply { init(kmf.keyManagers, null, null) }
        val s = context.serverSocketFactory.createServerSocket() as SSLServerSocket
        s.bind(InetSocketAddress(InetAddress.getByName("127.0.0.1"), 0))
        server = s
        thread(isDaemon = true) {
            runCatching {
                s.accept().use { client ->
                    val input = client.getInputStream()
                    val head = StringBuilder()
                    while (!head.endsWith("\r\n\r\n")) head.append(input.read().toChar())
                    val wsKey = head.lines().first { it.startsWith("Sec-WebSocket-Key:") }.substringAfter(':').trim()
                    val accept = Base64.getEncoder().encodeToString(
                        MessageDigest.getInstance("SHA-1").digest((wsKey + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").toByteArray()),
                    )
                    val out = client.getOutputStream()
                    out.write(
                        ("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
                            "Sec-WebSocket-Accept: $accept\r\n\r\n").toByteArray(),
                    )
                    out.write(frame)
                    out.flush()
                    input.read() // hold the connection until the client gives up
                }
            }
        }
        return s.localPort
    }

    private fun connect(port: Int) =
        PinnedWebSocket.connect("127.0.0.1", port, PAIR_PATH, Pins.of(cert), readTimeoutMillis = 10_000)

    /** A text frame with a 64-bit length whose top bit is set: negative as a signed long. */
    private val negativeLength = byteArrayOf(0x81.toByte(), 127) + ByteArray(8) { 0xff.toByte() }

    @Test fun negativeFrameLengthIsAnIoError() {
        connect(hostilePc(negativeLength)).use { ws ->
            try {
                ws.receive()
                fail("accepted a negative frame length")
            } catch (e: IOException) {
                assertEquals("bad frame length", e.message)
            }
        }
    }

    @Test fun negativeFrameLengthFailsThePairingCall() {
        val channel = WebSocketPairingChannel(connect(hostilePc(negativeLength)))
        channel.use {
            try {
                runBlocking { it.call("pair_qr", JsonObject(emptyMap()), 10.seconds) }
                fail("call succeeded")
            } catch (_: IOException) {
                // Expected: the reader failed and reported it, without crashing the app.
            }
        }
    }
}
