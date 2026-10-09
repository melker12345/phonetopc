package io.github.melker12345.phonetopc.server

import io.github.melker12345.phonetopc.protocol.security.Pins
import java.io.IOException
import java.io.InputStream
import java.io.OutputStream
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.net.SocketException
import java.security.Principal
import java.security.PrivateKey
import java.security.SecureRandom
import java.security.cert.X509Certificate
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicInteger
import javax.net.ssl.SSLContext
import javax.net.ssl.SSLEngine
import javax.net.ssl.SSLServerSocket
import javax.net.ssl.SSLSocket
import javax.net.ssl.X509ExtendedKeyManager

/**
 * The phone's long-term TLS identity (SECURITY.md §2): an ECDSA P-256 key and its self-signed
 * certificate. On Android [privateKey] is a non-exportable Android Keystore key; the platform
 * TLS stack (Conscrypt) signs with it through Keystore, so the key never leaves it.
 */
class TlsIdentity(val privateKey: PrivateKey, val certificate: X509Certificate) {
    /** `sha256/…` over the certificate's SubjectPublicKeyInfo. */
    val pin: String = Pins.of(certificate)

    /** A server-side TLS context with exactly this key. */
    fun serverContext(): SSLContext = SSLContext.getInstance("TLS").apply {
        init(arrayOf(SingleKeyManager(privateKey, certificate)), null, SecureRandom())
    }
}

/** Hands the TLS stack our one key, whatever it asks for. Works with opaque Keystore keys. */
private class SingleKeyManager(
    private val key: PrivateKey,
    private val cert: X509Certificate,
) : X509ExtendedKeyManager() {
    override fun getServerAliases(keyType: String?, issuers: Array<out Principal>?) = arrayOf(ALIAS)
    override fun chooseServerAlias(keyType: String?, issuers: Array<out Principal>?, socket: Socket?) = ALIAS
    override fun chooseEngineServerAlias(keyType: String?, issuers: Array<out Principal>?, engine: SSLEngine?) = ALIAS
    override fun getCertificateChain(alias: String?) = arrayOf(cert)
    override fun getPrivateKey(alias: String?) = key
    override fun getClientAliases(keyType: String?, issuers: Array<out Principal>?): Array<String>? = null
    override fun chooseClientAlias(keyType: Array<out String>?, issuers: Array<out Principal>?, socket: Socket?): String? = null

    private companion object {
        const val ALIAS = "phonetopc"
    }
}

/** Who is really on the other end of a connection the [TlsFrontend] relays. */
data class Peer(val host: String, val port: Int) {
    override fun toString() = if (host.contains(':')) "[$host]:$port" else "$host:$port"
}

/**
 * Terminates TLS for the WebSocket server.
 *
 * Ktor's CIO engine can't serve TLS, so the phone runs it on a loopback port and puts this in
 * front: an [SSLServerSocket] on the public port that relays each decrypted connection to
 * the loopback server. [peerFor] maps the loopback connection back to the real client, which
 * the server needs for rate limiting and must have: a loopback connection that didn't come
 * through here (another app on the phone) is closed before it can say anything.
 *
 * TLS 1.3 and 1.2 are enabled (SECURITY.md §3); with an EC key every 1.2 suite on offer is
 * ECDHE_ECDSA, and only AEAD suites are allowed. Blocking IO, two threads per connection;
 * connections are few.
 */
class TlsFrontend(
    private val identity: TlsIdentity,
    private val host: String,
    private val port: Int,
    private val backendPort: Int,
    /**
     * The loopback address the backend is bound to. Must match exactly: on Android
     * `InetAddress.getLoopbackAddress()` is `::1`, while the backend binds `127.0.0.1`.
     */
    private val backendHost: String = "127.0.0.1",
    /** A handshake (and the WebSocket upgrade's first bytes) must finish within this. */
    private val handshakeTimeoutMillis: Int = 10_000,
    private val maxConnections: Int = 64,
    /** Called for relay failures after a successful handshake; those are bugs, not bad clients. */
    private val onError: (String, Throwable) -> Unit = { msg, e -> System.err.println("phonetopc tls: $msg: $e") },
) {
    private var server: SSLServerSocket? = null
    private val peers = ConcurrentHashMap<Int, Peer>()
    private val open = ConcurrentHashMap.newKeySet<Socket>()
    private val active = AtomicInteger()
    private val threadId = AtomicInteger()
    private val pool: ExecutorService = Executors.newCachedThreadPool { r ->
        Thread(r, "tls-${threadId.incrementAndGet()}").apply { isDaemon = true }
    }

    val boundPort: Int get() = requireNotNull(server) { "not started" }.localPort

    /** The real client behind a loopback connection from this frontend, by its loopback source port. */
    fun peerFor(loopbackPort: Int): Peer? = peers[loopbackPort]

    /** Binds; throws if the port is taken. */
    fun start() {
        val context = identity.serverContext()
        val socket = context.serverSocketFactory.createServerSocket() as SSLServerSocket
        try {
            socket.reuseAddress = true
            socket.bind(InetSocketAddress(InetAddress.getByName(host), port))
            socket.enabledProtocols = PROTOCOLS.filter { it in socket.supportedProtocols }.toTypedArray()
            socket.enabledCipherSuites = socket.supportedCipherSuites.filter(::isAllowedSuite).toTypedArray()
            socket.needClientAuth = false
        } catch (e: Exception) {
            socket.close()
            throw e
        }
        server = socket
        pool.execute { acceptLoop(socket) }
    }

    fun stop() {
        runCatching { server?.close() }
        open.forEach { runCatching { it.close() } }
        open.clear()
        pool.shutdownNow()
    }

    private fun acceptLoop(server: SSLServerSocket) {
        while (!server.isClosed) {
            val client = try {
                server.accept() as SSLSocket
            } catch (e: SocketException) {
                break // closed
            } catch (e: IOException) {
                continue
            }
            if (active.incrementAndGet() > maxConnections) {
                active.decrementAndGet()
                runCatching { client.close() }
                continue
            }
            try {
                pool.execute { relay(client) }
            } catch (e: Exception) {
                active.decrementAndGet()
                runCatching { client.close() }
            }
        }
    }

    private fun relay(client: SSLSocket) {
        open += client
        var backend: Socket? = null
        var loopbackPort = -1
        try {
            client.tcpNoDelay = true
            client.soTimeout = handshakeTimeoutMillis
            client.startHandshake()
            client.soTimeout = 0 // the WebSocket layer pings and times out idle clients

            val b = Socket()
            backend = b
            open += b
            b.tcpNoDelay = true
            try {
                b.connect(InetSocketAddress(InetAddress.getByName(backendHost), backendPort), handshakeTimeoutMillis)
            } catch (e: IOException) {
                onError("can't reach the backend at $backendHost:$backendPort", e)
                throw e
            }
            loopbackPort = b.localPort
            peers[loopbackPort] = Peer(client.inetAddress.hostAddress ?: "?", client.port)

            val back = pool.submit { pump(b.getInputStream(), client.getOutputStream()); closeBoth(client, b) }
            pump(client.getInputStream(), b.getOutputStream())
            closeBoth(client, b)
            back.cancel(true)
        } catch (_: IOException) {
            // Failed handshakes (scanners, plain ws:// clients, wrong pins) land here.
        } catch (e: RuntimeException) {
            onError("relay failed", e)
        } finally {
            if (loopbackPort >= 0) peers.remove(loopbackPort)
            closeBoth(client, backend)
            open -= client
            backend?.let { open -= it }
            active.decrementAndGet()
        }
    }

    private fun pump(from: InputStream, to: OutputStream) {
        val buf = ByteArray(16 * 1024)
        try {
            while (true) {
                val n = from.read(buf)
                if (n < 0) break
                to.write(buf, 0, n)
                to.flush()
            }
        } catch (_: IOException) {
        }
    }

    private fun closeBoth(a: Socket, b: Socket?) {
        runCatching { a.close() }
        runCatching { b?.close() }
    }

    companion object {
        val PROTOCOLS = listOf("TLSv1.3", "TLSv1.2")

        /** TLS 1.3 suites (all AEAD) and TLS 1.2 ECDHE suites with AES-GCM or ChaCha20-Poly1305. */
        fun isAllowedSuite(suite: String): Boolean =
            suite.startsWith("TLS_AES_") || suite.startsWith("TLS_CHACHA20_") ||
                (suite.startsWith("TLS_ECDHE_") && (suite.contains("_GCM_") || suite.contains("CHACHA20_POLY1305")))
    }
}
