package io.github.melker12345.phonetopc.protocol

import io.github.melker12345.phonetopc.protocol.approval.ApprovalService
import io.github.melker12345.phonetopc.protocol.security.HelloRateLimiter
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.util.concurrent.atomic.AtomicLong

data class SubscriptionSnapshot(
    val id: String,
    val source: String,
    val type: String?,
    val rateHz: Double?,
)

/** What the UI shows about one connected client. */
data class ClientSnapshot(
    val sessionId: Long,
    val remote: String,
    val clientName: String?,
    val clientVersion: String?,
    val helloDone: Boolean,
    val subscriptions: List<SubscriptionSnapshot>,
    /** Secure mode: the paired PC this session authenticated as. */
    val pc: PcInfo? = null,
)

/**
 * Secure mode (SECURITY.md §5): `hello` must carry a token of a paired PC.
 *
 * @param authenticate maps a token to the PC it was issued to, or `null`. Must compare in
 *   constant time (see [io.github.melker12345.phonetopc.protocol.security.TokenHash]).
 * @param onAuthenticated called after every successful hello, e.g. to record "last connected".
 */
class SecureAccess(
    val authenticate: (token: String) -> PcInfo?,
    val limiter: HelloRateLimiter = HelloRateLimiter(),
    val onAuthenticated: (PcInfo) -> Unit = {},
)

/**
 * Owns all sessions and subscriptions, drives the [SensorSource] and fans readings out.
 *
 * A hardware source is only active while at least one subscription needs it. For continuous
 * sources it runs at the highest rate requested across all subscriptions; each subscription
 * then downsamples to its own rate.
 */
class BridgeHub(
    private val source: SensorSource,
    val serverInfo: ServerInfo,
    private val deviceInfoProvider: () -> DeviceInfo,
    val continuousQueueCapacity: Int = 512,
    val onChangeQueueCapacity: Int = 1024,
    /** Unsent responses a session may have before it is closed with 1008. */
    val responseQueueCapacity: Int = 256,
    /** Frames a session may send before a successful `hello`; one more closes it with 1008. */
    val framesBeforeHello: Int = 16,
    /** `null` is dev mode: no authentication, draft-1 behaviour. */
    val security: SecureAccess? = null,
    /**
     * Fingerprint approval (APPROVAL.md); only used in secure mode. `null`: the phone can't do
     * approvals, and `request_approval` answers `unavailable`.
     */
    val approvals: ApprovalService? = null,
) : ReadingSink {
    private val lock = Any()
    private val sessions = LinkedHashMap<Long, Session>()
    private val subsBySource = HashMap<String, MutableList<Subscription>>()
    private var appliedDemand: Map<String, Demand> = emptyMap()

    /** Copy-on-write routing table read by sensor threads without locking. */
    @Volatile private var routes: Map<String, List<Subscription>> = emptyMap()

    private val nextSessionId = AtomicLong(1)
    private val nextSubscriptionId = AtomicLong(1)

    private val _clients = MutableStateFlow<List<ClientSnapshot>>(emptyList())
    val clients: StateFlow<List<ClientSnapshot>> = _clients.asStateFlow()

    private val _demand = MutableStateFlow<Map<String, Demand>>(emptyMap())

    /** Sources currently running and at which rate. */
    val demand: StateFlow<Map<String, Demand>> = _demand.asStateFlow()

    private val _paused = MutableStateFlow(false)
    val paused: StateFlow<Boolean> = _paused.asStateFlow()

    val deviceId: String by lazy { deviceInfoProvider().id }

    init {
        source.setSink(this)
    }

    fun deviceInfo(): DeviceInfo = deviceInfoProvider()

    /**
     * @param remote shown in the UI, e.g. `192.168.1.40:51234`.
     * @param remoteHost the source address failed hellos are rate-limited by.
     */
    fun openSession(remote: String, remoteHost: String = remote): Session {
        val session = Session(this, nextSessionId.getAndIncrement(), remote, remoteHost)
        synchronized(lock) {
            sessions[session.sessionId] = session
            publishClientsLocked()
        }
        // SECURITY.md §5: an address over the failed-hello limit is closed with 1013 right away.
        if (security?.limiter?.isBlocked(remoteHost) == true) {
            session.requestClose(CloseRequest(CloseCodes.TRY_AGAIN_LATER, "too many failed hellos; try again later"))
        }
        return session
    }

    /**
     * Closes, with 1008, every session that authenticated as a PC matching [predicate], e.g.
     * one the user just revoked (SECURITY.md §5). Returns how many were closed.
     */
    fun disconnectPcs(predicate: (PcInfo) -> Boolean): Int = disconnectSessions { pc, _ -> predicate(pc) }

    /**
     * Like [disconnectPcs], but also sees the hash of the token each session used
     * ([io.github.melker12345.phonetopc.protocol.security.TokenHash]). Re-pairing closes only
     * the sessions that used the replaced token (SECURITY.md §4.4).
     */
    fun disconnectSessions(predicate: (pc: PcInfo, tokenHash: String?) -> Boolean): Int {
        val victims = synchronized(lock) {
            sessions.values.filter { s -> s.pc?.let { predicate(it, s.tokenHash) } == true }
        }
        victims.forEach { it.revoke() }
        return victims.size
    }

    /** Every catalog source, with availability and rates from the platform. */
    fun describe(): List<SensorInfo> {
        val platform = source.describe().associateBy { it.source }
        return SensorCatalog.all.map { spec -> platform[spec.source] ?: SensorInfo.of(spec, available = false) }
    }

    fun info(sourceName: String): SensorInfo {
        val spec = requireNotNull(SensorCatalog[sourceName]) { "not in catalog: $sourceName" }
        return source.describe().firstOrNull { it.source == sourceName } ?: SensorInfo.of(spec, available = false)
    }

    suspend fun read(sourceName: String): Reading? = source.read(sourceName)

    /** Pause stops all sources and event delivery; clients stay connected. */
    fun setPaused(paused: Boolean) {
        synchronized(lock) {
            _paused.value = paused
            recomputeLocked()
        }
    }

    /** Call when availability changed (e.g. the user toggled a sensor). */
    fun refreshAvailability() {
        synchronized(lock) { recomputeLocked() }
    }

    /** Stop every source and detach. Sessions are closed by the transport. */
    fun shutdown() {
        approvals?.cancelAll()
        synchronized(lock) {
            for (src in appliedDemand.keys) source.setDemand(src, null)
            appliedDemand = emptyMap()
            routes = emptyMap()
            _demand.value = emptyMap()
        }
        source.setSink(null)
    }

    override fun onReading(reading: Reading) {
        val subs = routes[reading.source] ?: return
        val id = deviceId
        for (sub in subs) sub.offer(reading, id)
    }

    internal fun newSubscriptionId(): String = "s-%04x".format(nextSubscriptionId.getAndIncrement())

    internal fun addSubscription(sub: Subscription) {
        synchronized(lock) {
            subsBySource.getOrPut(sub.spec.source) { mutableListOf() }.add(sub)
            recomputeLocked()
            publishClientsLocked()
        }
    }

    /**
     * Let a new subscription deliver events, starting with the current value of a stateful
     * source if one is known (spec §4.3). Call after its `subscribe` response is queued.
     * Nothing initial is sent while the source isn't running (paused, unavailable); the
     * first reading after it starts becomes the initial event instead.
     */
    internal fun startSubscription(sub: Subscription) {
        val routed = routes[sub.spec.source]?.contains(sub) == true
        val current = if (sub.spec.stateful && routed) source.currentReading(sub.spec.source) else null
        sub.start(current, deviceId)
    }

    internal fun removeSubscriptions(subs: List<Subscription>) {
        if (subs.isEmpty()) return
        synchronized(lock) {
            for (sub in subs) {
                val list = subsBySource[sub.spec.source] ?: continue
                list.remove(sub)
                if (list.isEmpty()) subsBySource.remove(sub.spec.source)
            }
            recomputeLocked()
            publishClientsLocked()
        }
    }

    internal fun closeSession(session: Session) {
        synchronized(lock) {
            sessions.remove(session.sessionId)
            publishClientsLocked()
        }
    }

    internal fun sessionChanged() {
        synchronized(lock) { publishClientsLocked() }
    }

    private fun recomputeLocked() {
        val available = describe().filter { it.available }.mapTo(HashSet()) { it.source }
        val wanted = HashMap<String, Demand>()
        val newRoutes = HashMap<String, List<Subscription>>()
        if (!_paused.value) {
            for ((src, subs) in subsBySource) {
                if (subs.isEmpty() || src !in available) continue
                val spec = SensorCatalog[src] ?: continue
                wanted[src] = if (spec.mode == SensorMode.CONTINUOUS) {
                    Demand(subs.maxOf { it.rateHz ?: 0.0 })
                } else {
                    Demand(null)
                }
                newRoutes[src] = subs.toList()
            }
        }
        // Route first: a source may deliver its first reading as soon as it is started.
        routes = newRoutes
        for (src in appliedDemand.keys - wanted.keys) source.setDemand(src, null)
        for ((src, d) in wanted) if (appliedDemand[src] != d) source.setDemand(src, d)
        appliedDemand = wanted
        _demand.value = wanted.toMap()
    }

    private fun publishClientsLocked() {
        _clients.value = sessions.values.map { it.snapshot() }
    }
}
