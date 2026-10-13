package io.github.melker12345.phonetopc.protocol.panels

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject

/**
 * The PC that notifications and panels belong to (PANELS.md §1): a paired PC in secure mode, or
 * the one dev-mode PC that every dev-mode client counts as.
 *
 * @param id the paired PC's id, `null` for the dev-mode PC.
 * @param name the name the phone stored at pairing, never one the PC program chose.
 */
data class PcRef(val id: String?, val name: String) {
    val isDevMode: Boolean get() = id == null

    /** Unique across paired PCs and the dev-mode PC. */
    val key: String get() = if (id == null) DEV_KEY else "pc/$id"

    companion object {
        const val DEV_KEY = "dev"
        val DEV = PcRef(null, "Unverified PC (dev mode)")

        fun of(pc: PcInfo?): PcRef = if (pc == null) DEV else PcRef(pc.id, pc.name)
    }
}

/** The two features a user allows per PC (PANELS.md §1). */
enum class Feature(val wireName: String) {
    NOTIFY("notify"),
    PANELS("panels"),
}

/** The user's per-PC choices. Called often and from any thread: keep it cheap. Off by default. */
fun interface OutputPermissions {
    fun allowed(pc: PcRef, feature: Feature): Boolean
}

internal fun notAllowed(feature: Feature): Nothing = throw RpcException(
    RpcError.NOT_ALLOWED,
    "The user hasn't allowed ${if (feature == Feature.NOTIFY) "notifications" else "panels"} for this PC; turn them on in the phone app",
    buildJsonObject { put("permission", JsonPrimitive(feature.wireName)) },
)

/** Time for panels and notifications; replaceable in tests. */
interface OutputClock {
    /** Monotonic milliseconds, for expiry and rate limits. */
    fun elapsedMillis(): Long

    /** Wall-clock Unix milliseconds, an event's `ts`. */
    fun wallMillis(): Long

    /** Monotonic nanoseconds, an event's `ts_mono_ns`. */
    fun monoNanos(): Long

    object System : OutputClock {
        override fun elapsedMillis() = java.lang.System.nanoTime() / 1_000_000
        override fun wallMillis() = java.lang.System.currentTimeMillis()
        override fun monoNanos() = java.lang.System.nanoTime()
    }
}
