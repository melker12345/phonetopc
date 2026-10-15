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
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
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
    val pin: String = Pins.of(certificate)

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

/** What the WebSocket server learns from, and tells, the [TlsFrontend] about a connection it relays. */
interface Relays {
    /** The real client behind a loopback connection, or `null` if it didn't come through here. */
    fun peerFor(loopbackPort: Int): Peer?

    /** The client authenticated, so it no longer counts against its address's limit. */
    fun authenticated(loopbackPort: Int)

    /**
     * Drops the client at once, unsent data and all. For a client that stopped reading: a
     * normal close would wait for it forever.
     */
    fun drop(loopbackPort: Int)
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
 *
 * At most [maxConnections] at a time. A connection must get through the TLS handshake and the
 * WebSocket upgrade (until the server first answers) within [handshakeTimeoutMillis] in total,
 * however slowly it trickles in; after that the server's hello timeout takes over. One address
 * may hold at most [maxUnauthenticatedPerHost] connections that haven't authenticated yet
 * ([Relays.authenticated]), so it can't take every slot. Loopback addresses are exempt by
 * default: every `adb forward` client comes from 127.0.0.1.
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
    /** A handshake and the WebSocket upgrade must finish within this, in total. */
    private val handshakeTimeoutMillis: Int = 10_000,
    private val maxConnections: Int = 64,
    private val maxUnauthenticatedPerHost: Int = 8,
    /** Addresses without a per-address limit. */
    private val unlimited: (InetAddress) -> Boolean = { it.isLoopbackAddress },
    /** Called for relay failures after a successful handshake; those are bugs, not bad clients. */
    private val onError: (String, Throwable) -> Unit = { msg, e -> System.err.println("phonetopc tls: $msg: $e") },
) : Relays {
    /** One relayed connection; [host] is `null` for an address without a limit. */
    private class Client(val socket: SSLSocket, val host: String?) {
        val unauthenticated = AtomicBoolean(true)
    }

    private var server: SSLServerSocket? = null
    private val peers = ConcurrentHashMap<Int, Peer>()
    private val clients = ConcurrentHashMap<Int, Client>()
    private val open = ConcurrentHashMap.newKeySet<Socket>()
    private val active = AtomicInteger()
    private val unauthenticated = ConcurrentHashMap<String, Int>()
    private val threadId = AtomicInteger()
    private val pool: ExecutorService = Executors.newCachedThreadPool { r ->
        Thread(r, "tls-${threadId.incrementAndGet()}").apply { isDaemon = true }
    }
    private val deadlines: ScheduledExecutorService = Executors.newSingleThreadScheduledExecutor { r ->
        Thread(r, "tls-deadlines").apply { isDaemon = true }
    }

    val boundPort: Int get() = requireNotNull(server) { "not started" }.localPort

    override fun peerFor(loopbackPort: Int): Peer? = peers[loopbackPort]

    override fun authenticated(loopbackPort: Int) {
        clients[loopbackPort]?.let(::release)
    }

    override fun drop(loopbackPort: Int) {
        val client = clients[loopbackPort] ?: return
        // Off the caller's thread, since closing a TLS socket can still block.
        runCatching { pool.execute { abort(client.socket) } }
    }

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
        deadlines.shutdownNow()
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
            val host = client.inetAddress.takeUnless(unlimited)?.let { it.hostAddress ?: "?" }
            if (active.incrementAndGet() > maxConnections) {
                active.decrementAndGet()
                runCatching { client.close() }
                continue
            }
            if (host != null && !admit(host)) {
                active.decrementAndGet()
                runCatching { client.close() }
                continue
            }
            val c = Client(client, host)
            try {
                pool.execute { relay(c) }
            } catch (e: Exception) {
                release(c)
                active.decrementAndGet()
                runCatching { client.close() }
            }
        }
    }

    /** Counts an unauthenticated connection from [host], unless it already has the most allowed. */
    private fun admit(host: String): Boolean {
        var admitted = false
        unauthenticated.compute(host) { _, n ->
            val count = n ?: 0
            if (count < maxUnauthenticatedPerHost) {
                admitted = true
                count + 1
            } else {
                count
            }
        }
        return admitted
    }

    /** Stops counting [client] as unauthenticated (it authenticated or went away). Idempotent. */
    private fun release(client: Client) {
        val host = client.host ?: return
        if (!client.unauthenticated.compareAndSet(true, false)) return
        unauthenticated.computeIfPresent(host) { _, n -> (n - 1).takeIf { it > 0 } }
    }

    private fun relay(c: Client) {
        val client = c.socket
        open += client
        var backend: Socket? = null
        var loopbackPort = -1
        // Cut the connection unless the server has answered the upgrade by then.
        val deadline = try {
            deadlines.schedule({ abort(client) }, handshakeTimeoutMillis.toLong(), TimeUnit.MILLISECONDS)
        } catch (e: Exception) {
            null
        }
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
            clients[loopbackPort] = c

            val back = pool.submit {
                pump(b.getInputStream(), client.getOutputStream(), onFirst = { deadline?.cancel(false) })
                closeBoth(client, b)
            }
            pump(client.getInputStream(), b.getOutputStream())
            closeBoth(client, b)
            back.cancel(true)
        } catch (_: IOException) {
            // Failed handshakes (scanners, plain ws:// clients, wrong pins) land here.
        } catch (e: RuntimeException) {
            onError("relay failed", e)
        } finally {
            deadline?.cancel(false)
            release(c)
            if (loopbackPort >= 0) {
                peers.remove(loopbackPort)
                clients.remove(loopbackPort)
            }
            closeBoth(client, backend)
            open -= client
            backend?.let { open -= it }
            active.decrementAndGet()
        }
    }

    /** Copies until either side fails; [onFirst] runs once, when the first bytes arrive. */
    private fun pump(from: InputStream, to: OutputStream, onFirst: (() -> Unit)? = null) {
        val buf = ByteArray(16 * 1024)
        var first = onFirst
        try {
            while (true) {
                val n = from.read(buf)
                if (n < 0) break
                first?.invoke()
                first = null
                to.write(buf, 0, n)
                to.flush()
            }
        } catch (_: IOException) {
        }
    }

    /** Closes [socket] without waiting for a TLS close_notify or unsent bytes: SO_LINGER 0 resets it. */
    private fun abort(socket: Socket) {
        runCatching { socket.setSoLinger(true, 0) }
        runCatching { socket.close() }
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
