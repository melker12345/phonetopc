package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.security.Pem
import io.github.melker12345.phonetopc.protocol.security.Pins
import org.junit.After
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.File
import java.io.IOException
import java.net.InetAddress
import java.net.ServerSocket
import java.net.Socket
import java.net.SocketTimeoutException
import java.security.cert.X509Certificate
import java.util.concurrent.CopyOnWriteArrayList
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509TrustManager
import kotlin.concurrent.thread

/** [TlsFrontend] against clients that hold slots: slow handshakes, slow upgrades, many sockets. */
class TlsFrontendTest {
    private val vectors = File(System.getProperty("phonetopc.conformanceDir") ?: "../../spec/conformance", "vectors")
    private val identity = TlsIdentity(
        Pem.ecPrivateKey(File(vectors, "phone-key.pem").readText()),
        Pins.parsePemCertificate(File(vectors, "phone-cert.pem").readText()),
    )

    /** A backend that answers each connection's first byte with [answer] (or never, if `null`). */
    private class Backend(answer: String?) : AutoCloseable {
        val socket = ServerSocket(0, 50, InetAddress.getByName("127.0.0.1"))

        /** Loopback source ports of the frontend's connections, in order. */
        val loopbackPorts = CopyOnWriteArrayList<Int>()

        init {
            thread(isDaemon = true) {
                while (true) {
                    val s = runCatching { socket.accept() }.getOrNull() ?: break
                    loopbackPorts += s.port
                    thread(isDaemon = true) {
                        runCatching {
                            if (s.getInputStream().read() >= 0 && answer != null) s.getOutputStream().write(answer.toByteArray())
                            s.getInputStream().readBytes()
                        }
                    }
                }
            }
        }

        override fun close() = socket.close()
    }

    private var frontend: TlsFrontend? = null
    private var backend: Backend? = null

    @After fun tearDown() {
        frontend?.stop()
        backend?.close()
    }

    private fun start(
        answer: String? = "HTTP/1.1 101 Switching Protocols\r\n\r\n",
        timeoutMillis: Int = 10_000,
        perHost: Int = 8,
        unlimited: (InetAddress) -> Boolean = { it.isLoopbackAddress },
    ): Int {
        val b = Backend(answer).also { backend = it }
        val f = TlsFrontend(
            identity, "127.0.0.1", 0, b.socket.localPort,
            handshakeTimeoutMillis = timeoutMillis,
            maxUnauthenticatedPerHost = perHost,
            unlimited = unlimited,
        )
        f.start()
        frontend = f
        return f.boundPort
    }

    private fun tls(port: Int): SSLSocket {
        val trustAll = object : X509TrustManager {
            override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
            override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) {}
            override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
        }
        val context = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trustAll), null) }
        return (context.socketFactory.createSocket("127.0.0.1", port) as SSLSocket).apply { startHandshake() }
    }

    /** Whether the frontend closed [socket] within [millis] (reading it then ends or fails). */
    private fun closedWithin(socket: Socket, millis: Int): Boolean {
        socket.soTimeout = millis
        return try {
            while (socket.getInputStream().read() >= 0) {}
            true
        } catch (_: SocketTimeoutException) {
            false
        } catch (_: IOException) {
            true
        }
    }

    @Test fun handshakeTricklingInIsCutAtTheDeadline() {
        val port = start(timeoutMillis = 500)
        Socket("127.0.0.1", port).use { s ->
            // A plausible TLS record header, then one byte at a time: each read succeeds within
            // the per-read timeout, but the whole handshake never finishes.
            val writer = thread(isDaemon = true) {
                runCatching {
                    s.getOutputStream().write(byteArrayOf(0x16, 0x03, 0x01, 0x02, 0x00))
                    repeat(100) {
                        s.getOutputStream().write(1)
                        Thread.sleep(100)
                    }
                }
            }
            val start = System.nanoTime()
            assertTrue("still open", closedWithin(s, 5_000))
            assertTrue((System.nanoTime() - start) / 1_000_000 < 3_000)
            writer.interrupt()
        }
    }

    @Test fun upgradeThatNeverEndsIsCutAtTheDeadline() {
        val port = start(answer = null, timeoutMillis = 500)
        tls(port).use { s ->
            s.getOutputStream().write("GET /v1/ws HTTP/1.1\r\n".toByteArray())
            s.getOutputStream().flush()
            assertTrue("still open", closedWithin(s, 5_000))
        }
    }

    @Test fun answeredUpgradeOutlivesTheDeadline() {
        val port = start(timeoutMillis = 300)
        tls(port).use { s ->
            s.getOutputStream().write("GET /v1/ws HTTP/1.1\r\n\r\n".toByteArray())
            s.getOutputStream().flush()
            assertTrue("closed", !closedWithin(s, 1_500))
        }
    }

    @Test fun oneAddressCantTakeEverySlot() {
        val port = start(perHost = 2, unlimited = { false })
        val held = List(2) { Socket("127.0.0.1", port) }
        try {
            held.forEach { assertTrue("refused too early", !closedWithin(it, 300)) }
            Socket("127.0.0.1", port).use { assertTrue("third was let in", closedWithin(it, 2_000)) }
            // A slot freed by a client that left can be used again.
            held[0].close()
            Thread.sleep(200)
            Socket("127.0.0.1", port).use { assertTrue("refused after one left", !closedWithin(it, 300)) }
        } finally {
            held.forEach { it.close() }
        }
    }

    @Test fun authenticatedClientsDontCountAgainstTheirAddress() {
        val port = start(perHost = 1, unlimited = { false })
        tls(port).use { first ->
            first.getOutputStream().write("GET /v1/ws HTTP/1.1\r\n\r\n".toByteArray())
            first.getOutputStream().flush()
            val deadline = System.currentTimeMillis() + 2_000
            while (backend!!.loopbackPorts.isEmpty() && System.currentTimeMillis() < deadline) Thread.sleep(10)
            frontend!!.authenticated(backend!!.loopbackPorts.single())
            tls(port).use { second -> assertTrue("refused", !closedWithin(second, 300)) }
        }
    }

    @Test fun loopbackHasNoPerAddressLimit() {
        val port = start(perHost = 1)
        val held = List(3) { Socket("127.0.0.1", port) }
        try {
            held.forEach { assertTrue("refused", !closedWithin(it, 300)) }
        } finally {
            held.forEach { it.close() }
        }
    }
}
