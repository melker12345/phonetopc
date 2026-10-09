package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.security.Pins
import java.io.BufferedInputStream
import java.io.ByteArrayOutputStream
import java.io.Closeable
import java.io.EOFException
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.Socket
import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateException
import java.security.cert.X509Certificate
import java.util.Base64
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLEngine
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509ExtendedTrustManager

/** The server's certificate didn't have the expected pin (SECURITY.md §3: a hard failure). */
class PinMismatchException(val expected: String, val seen: String) :
    IOException("Certificate pin mismatch: expected $expected, got $seen")

/**
 * A minimal RFC 6455 WebSocket client over TLS, with the server authenticated only by its SPKI
 * pin (SECURITY.md §2): no chains, names or expiry are checked. Text frames only, which is all
 * pairing needs. Uses the platform TLS stack (Conscrypt on Android), so TLS 1.3 where available
 * and TLS 1.2 otherwise.
 *
 * Blocking; call from an IO thread. [send] may be called from any thread.
 */
class PinnedWebSocket private constructor(
    private val socket: Socket,
    /** The pin of the certificate the server presented. */
    val pinSeen: String,
) : Closeable {
    private val input: InputStream = BufferedInputStream(socket.getInputStream())
    private val output: OutputStream = socket.getOutputStream()
    private val random = SecureRandom()
    @Volatile private var closed = false

    /** The close code the server sent, once it closed the connection. */
    @Volatile var closeCode: Int? = null
        private set

    /** Sends one text message. */
    fun send(text: String) = writeFrame(OP_TEXT, text.toByteArray(Charsets.UTF_8))

    /**
     * The next text message, or `null` when the server closed the connection. Answers pings,
     * ignores pongs and binary frames.
     */
    fun receive(): String? {
        val message = ByteArrayOutputStream()
        var collecting = false
        while (true) {
            val b0 = input.read()
            if (b0 < 0) return null
            val b1 = readByte()
            val fin = b0 and 0x80 != 0
            val opcode = b0 and 0x0f
            if (b1 and 0x80 != 0) throw IOException("server frames must not be masked")
            val len = when (val l = b1 and 0x7f) {
                126 -> (readByte() shl 8) or readByte()
                127 -> {
                    var v = 0L
                    repeat(8) { v = (v shl 8) or readByte().toLong() }
                    if (v > MAX_MESSAGE) throw IOException("frame too large")
                    v.toInt()
                }
                else -> l
            }
            if (len > MAX_MESSAGE) throw IOException("frame too large")
            val payload = readFully(len)
            when (opcode) {
                OP_TEXT, OP_CONT -> {
                    if (opcode == OP_TEXT) {
                        message.reset()
                        collecting = true
                    }
                    if (!collecting) continue
                    message.write(payload)
                    if (message.size() > MAX_MESSAGE) throw IOException("message too large")
                    if (fin) return message.toString(Charsets.UTF_8.name())
                }
                OP_BINARY -> collecting = false
                OP_PING -> writeFrame(OP_PONG, payload)
                OP_PONG -> Unit
                OP_CLOSE -> {
                    closeCode = if (payload.size >= 2) ((payload[0].toInt() and 0xff) shl 8) or (payload[1].toInt() and 0xff) else NO_STATUS
                    runCatching { writeFrame(OP_CLOSE, payload.take(2).toByteArray()) }
                    closed = true
                    runCatching { socket.close() }
                    return null
                }
                else -> throw IOException("unknown opcode $opcode")
            }
        }
    }

    /** Sends a close frame (best effort) and closes the socket. Idempotent. */
    override fun close() {
        if (closed) return
        runCatching { writeFrame(OP_CLOSE, byteArrayOf((NORMAL_CLOSURE shr 8).toByte(), NORMAL_CLOSURE.toByte())) }
        closed = true
        runCatching { socket.close() }
    }

    private fun readByte(): Int {
        val b = input.read()
        if (b < 0) throw EOFException()
        return b
    }

    private fun readFully(n: Int): ByteArray {
        val buf = ByteArray(n)
        var off = 0
        while (off < n) {
            val r = input.read(buf, off, n - off)
            if (r < 0) throw EOFException()
            off += r
        }
        return buf
    }

    @Synchronized
    private fun writeFrame(opcode: Int, payload: ByteArray) {
        if (closed) throw IOException("closed")
        val header = ByteArrayOutputStream()
        header.write(0x80 or opcode)
        when {
            payload.size < 126 -> header.write(0x80 or payload.size)
            payload.size <= 0xffff -> {
                header.write(0x80 or 126)
                header.write(payload.size shr 8)
                header.write(payload.size and 0xff)
            }
            else -> {
                header.write(0x80 or 127)
                for (shift in 56 downTo 0 step 8) header.write(((payload.size.toLong() shr shift) and 0xff).toInt())
            }
        }
        val mask = ByteArray(4).also { random.nextBytes(it) }
        header.write(mask)
        val masked = ByteArray(payload.size) { i -> (payload[i].toInt() xor mask[i and 3].toInt()).toByte() }
        output.write(header.toByteArray() + masked)
        output.flush()
    }

    companion object {
        private const val OP_CONT = 0
        private const val OP_TEXT = 1
        private const val OP_BINARY = 2
        private const val OP_CLOSE = 8
        private const val OP_PING = 9
        private const val OP_PONG = 10
        private const val NORMAL_CLOSURE = 1000
        private const val NO_STATUS = 1005
        private const val MAX_MESSAGE = 1 shl 20
        private const val GUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

        /** TLS versions we offer, newest first (SECURITY.md §3). */
        val PROTOCOLS = listOf("TLSv1.3", "TLSv1.2")

        /**
         * Connects to `wss://host:port/path`.
         *
         * @param expectedPin if set, the handshake fails with [PinMismatchException] unless the
         *   server's certificate has this pin. If `null`, any certificate is accepted and its pin
         *   is reported in [pinSeen] (pairing way B, SECURITY.md §4.3).
         * @param protocols TLS versions to enable, for tests.
         */
        fun connect(
            host: String,
            port: Int,
            path: String,
            expectedPin: String?,
            connectTimeoutMillis: Int = 5_000,
            readTimeoutMillis: Int = 0,
            protocols: List<String> = PROTOCOLS,
        ): PinnedWebSocket {
            val trust = PinTrustManager(expectedPin)
            val context = SSLContext.getInstance("TLS").apply { init(null, arrayOf(trust), SecureRandom()) }
            val raw = Socket()
            try {
                raw.connect(InetSocketAddress(host, port), connectTimeoutMillis)
                raw.tcpNoDelay = true
                raw.soTimeout = connectTimeoutMillis
                val ssl = context.socketFactory.createSocket(raw, host, port, true) as SSLSocket
                ssl.enabledProtocols = protocols.filter { it in ssl.supportedProtocols }.toTypedArray()
                try {
                    ssl.startHandshake()
                } catch (e: IOException) {
                    val seen = trust.seen
                    if (expectedPin != null && seen != null && !Pins.matches(seen, expectedPin)) {
                        throw PinMismatchException(expectedPin, seen)
                    }
                    throw e
                }
                val pin = trust.seen ?: throw IOException("no server certificate")
                upgrade(ssl, host, port, path)
                ssl.soTimeout = readTimeoutMillis
                return PinnedWebSocket(ssl, pin)
            } catch (e: Exception) {
                runCatching { raw.close() }
                throw e
            }
        }

        private fun upgrade(socket: SSLSocket, host: String, port: Int, path: String) {
            val key = Base64.getEncoder().encodeToString(ByteArray(16).also { SecureRandom().nextBytes(it) })
            val hostHeader = if (host.contains(':')) "[$host]:$port" else "$host:$port"
            val request = "GET $path HTTP/1.1\r\n" +
                "Host: $hostHeader\r\n" +
                "Upgrade: websocket\r\n" +
                "Connection: Upgrade\r\n" +
                "Sec-WebSocket-Key: $key\r\n" +
                "Sec-WebSocket-Version: 13\r\n\r\n"
            socket.outputStream.write(request.toByteArray(Charsets.US_ASCII))
            socket.outputStream.flush()

            // Read the response head byte by byte so no frame bytes are consumed.
            val head = StringBuilder()
            val input = socket.inputStream
            while (!head.endsWith("\r\n\r\n")) {
                val b = input.read()
                if (b < 0) throw EOFException("connection closed during upgrade")
                head.append(b.toChar())
                if (head.length > 8192) throw IOException("upgrade response too long")
            }
            val lines = head.split("\r\n")
            val status = lines.first().split(' ').getOrNull(1)
            if (status != "101") throw IOException("upgrade refused: ${lines.first()}")
            val accept = lines.drop(1).firstOrNull { it.startsWith("sec-websocket-accept:", ignoreCase = true) }
                ?.substringAfter(':')?.trim()
            val expected = Base64.getEncoder().encodeToString(
                MessageDigest.getInstance("SHA-1").digest((key + GUID).toByteArray(Charsets.US_ASCII)),
            )
            if (accept != expected) throw IOException("bad Sec-WebSocket-Accept")
        }
    }
}

/**
 * Accepts exactly one thing: a leaf certificate whose SPKI pin is [expected] (or any, when
 * `null`, recording what it saw). Deliberately no chain, name or date checks: trust comes only
 * from the pin (SECURITY.md §2).
 */
private class PinTrustManager(private val expected: String?) : X509ExtendedTrustManager() {
    @Volatile var seen: String? = null
        private set

    private fun check(chain: Array<out X509Certificate>?) {
        val leaf = chain?.firstOrNull() ?: throw CertificateException("no certificate")
        val pin = Pins.of(leaf)
        seen = pin
        if (expected != null && !Pins.matches(pin, expected)) throw CertificateException("pin mismatch")
    }

    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?) = check(chain)
    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?, socket: Socket?) = check(chain)
    override fun checkServerTrusted(chain: Array<out X509Certificate>?, authType: String?, engine: SSLEngine?) = check(chain)

    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?) =
        throw CertificateException("client certificates are not used")
    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?, socket: Socket?) =
        throw CertificateException("client certificates are not used")
    override fun checkClientTrusted(chain: Array<out X509Certificate>?, authType: String?, engine: SSLEngine?) =
        throw CertificateException("client certificates are not used")

    override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
}
