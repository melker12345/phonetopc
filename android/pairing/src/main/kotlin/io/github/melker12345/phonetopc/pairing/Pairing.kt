package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.lenientObj
import io.github.melker12345.phonetopc.protocol.lenientString
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.protocol.security.Sas
import io.github.melker12345.phonetopc.protocol.security.Secrets
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.TimeoutCancellationException
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonArray
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import java.io.IOException
import kotlin.time.Duration
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

const val PAIR_PROTOCOL = "phonetopc-pair/1"

/** What the phone tells the PC about itself (`phone` in `pair_qr` / `pair_sas_begin`). */
data class PhoneInfo(
    val id: String,
    val name: String,
    val pin: String,
    val port: Int,
    val addresses: List<String>,
)

/**
 * A finished pairing, handed to the store before the UI reports success. [token] is the
 * plain token the PC now holds; the phone should keep only a hash of it.
 */
data class NewPairing(val pcId: String, val pcName: String, val pin: String, val token: String)

/** A PC found by browsing `_phonetopc-pair._tcp` (pairing way B). */
data class NetworkPc(val name: String, val hosts: List<String>, val port: Int, val id: String? = null)

/** Pairing error codes (SECURITY.md §4.5). */
object PairingErrors {
    const val BAD_SECRET = -32020
    const val REJECTED = -32021
    const val OUT_OF_ORDER = -32022
    const val SAS_MISMATCH = -32023
    const val EXPIRED = -32024
    const val UNSUPPORTED_PROTOCOL = -32004
}

/** Why pairing stopped. Every case has its own message and a retry in the UI. */
sealed interface PairingFailure {
    /**
     * -32020: the PC didn't accept the QR secret. It shows a new QR code after any attempt
     * that reached its prompt (SECURITY.md §4.2), so the scanned one may be used up.
     */
    data object BadSecret : PairingFailure

    /** -32021: the PC's user declined. */
    data object Rejected : PairingFailure

    /** -32021 to `pair_sas_begin`: the PC only pairs by QR code (e.g. `phonetopc pair --yes`). */
    data object QrOnly : PairingFailure

    /** -32022: the two sides got out of step. */
    data object OutOfOrder : PairingFailure

    /** -32023, or we said no: the codes didn't match. */
    data object CodesDiffer : PairingFailure

    /** The PC's revealed nonce didn't match its commitment: someone is interfering. */
    data object CommitmentMismatch : PairingFailure

    /** -32024: the PC's pairing listener timed out. */
    data object Expired : PairingFailure

    /** -32004: the PC speaks another pairing protocol version. */
    data object UnsupportedProtocol : PairingFailure

    data object PinMismatch : PairingFailure

    /** None of the PC's addresses could be reached. */
    data class Unreachable(val detail: String?) : PairingFailure

    data class ConnectionLost(val detail: String?) : PairingFailure

    data object TimedOut : PairingFailure

    /** Any other error code, or a malformed answer. */
    data class Protocol(val detail: String) : PairingFailure
}

sealed interface PairingState {
    data object Idle : PairingState

    data class Connecting(val pcName: String?) : PairingState

    /** Way A: `pair_qr` sent, the PC's user is being asked to confirm. */
    data class WaitingForPcApproval(val pcName: String) : PairingState

    /** Way B: show [code] (6 digits) and ask the user whether the PC shows the same. */
    data class CompareCode(val pcName: String, val code: String) : PairingState

    /** Way B: we said they match; waiting for the PC's user to say the same. */
    data class WaitingForPcCode(val pcName: String, val code: String) : PairingState

    /** `pair_finish` sent. */
    data class Finishing(val pcName: String) : PairingState

    data class Paired(val pcId: String, val pcName: String) : PairingState

    data class Failed(val failure: PairingFailure, val pcName: String?) : PairingState
}

/**
 * The phone's side of pairing (SECURITY.md §4): a state machine driven by [pairWithQr] or
 * [pairOverNetwork]. One run at a time; cancel the calling coroutine to abort.
 *
 * @param store persists the new pairing. Called only after the PC answered `pair_finish`, and
 *   before [state] becomes [PairingState.Paired].
 */
class PairingClient(
    private val connector: PairingConnector,
    private val phone: suspend () -> PhoneInfo,
    private val store: suspend (NewPairing) -> Unit,
    private val requestTimeout: Duration = 15.seconds,
    /**
     * How long the PC's user may take to answer `pair_qr` and `pair_sas_confirm`. SECURITY.md
     * §4.1 asks for at least 5 minutes; a little more, so the PC's own 5-minute expiry
     * (`-32024` or close 1001) is what ends a forgotten pairing, not a race with ours.
     */
    private val userTimeout: Duration = USER_TIMEOUT,
) {
    private val _state = MutableStateFlow<PairingState>(PairingState.Idle)
    val state: StateFlow<PairingState> = _state.asStateFlow()

    @Volatile private var codeAnswer: CompletableDeferred<Boolean>? = null

    /** The user's answer to [PairingState.CompareCode]. */
    fun answerCode(match: Boolean) {
        codeAnswer?.complete(match)
    }

    fun reset() {
        _state.value = PairingState.Idle
    }

    /** Way A (§4.2). */
    suspend fun pairWithQr(qr: PairingQr) {
        run(qr.pcName) {
            val channel = connectAny(qr.pcName, qr.hosts, qr.port, qr.pin)
            channel.use { ch ->
                _state.value = PairingState.WaitingForPcApproval(qr.pcName)
                val result = ch.call(
                    "pair_qr",
                    buildJsonObject {
                        put("protocol", PAIR_PROTOCOL)
                        put("secret", qr.secret)
                        put("phone", phone().toJson())
                    },
                    userTimeout,
                )
                val pcName = result.lenientObj("pc")?.lenientString("name") ?: qr.pcName
                finish(ch, pin = qr.pin, fallbackId = qr.pcId, fallbackName = pcName)
            }
        }
    }

    /** Way B (§4.3). */
    suspend fun pairOverNetwork(pc: NetworkPc) {
        run(pc.name) {
            val channel = connectAny(pc.name, pc.hosts, pc.port, expectedPin = null)
            channel.use { ch ->
                val me = phone()
                val pcPinSeen = ch.pcPin
                val begin = try {
                    ch.call(
                        "pair_sas_begin",
                        buildJsonObject {
                            put("protocol", PAIR_PROTOCOL)
                            put("phone", me.toJson())
                        },
                        requestTimeout,
                    )
                } catch (e: PairingRpcException) {
                    // §4.3: a PC that pairs without asking its user refuses code comparison.
                    if (e.code == PairingErrors.REJECTED) throw Failure(PairingFailure.QrOnly)
                    throw e
                }
                val pcInfo = begin.lenientObj("pc")
                val pcName = pcInfo?.lenientString("name") ?: pc.name
                val pcId = pcInfo?.lenientString("id") ?: pc.id
                val commitment = begin.lenientString("commitment") ?: throw ProtocolError("pair_sas_begin result has no commitment")

                val nphone = Secrets.newNonce()
                val reveal = ch.call("pair_sas_nonce", buildJsonObject { put("nonce", Base64Url.encode(nphone)) }, requestTimeout)
                val npc = reveal.lenientString("nonce")?.let(Base64Url::decode)
                if (npc == null || npc.size != Secrets.NONCE_BYTES || !Sas.verifyCommitment(commitment, npc)) {
                    // §4.3: a failed commitment check (including a missing or malformed nonce)
                    // sends match:false, so the PC discards everything, then aborts.
                    runCatching { ch.call("pair_sas_confirm", buildJsonObject { put("match", false) }, SHORT) }
                    throw Failure(PairingFailure.CommitmentMismatch)
                }

                val code = Sas.code(pcPinSeen, me.pin, npc, nphone)
                val answer = CompletableDeferred<Boolean>()
                codeAnswer = answer
                _state.value = PairingState.CompareCode(pcName, code)
                val match = withTimeout(userTimeout) { answer.await() }
                codeAnswer = null
                if (!match) {
                    runCatching { ch.call("pair_sas_confirm", buildJsonObject { put("match", false) }, SHORT) }
                    throw Failure(PairingFailure.CodesDiffer)
                }
                _state.value = PairingState.WaitingForPcCode(pcName, code)
                ch.call("pair_sas_confirm", buildJsonObject { put("match", true) }, userTimeout)
                finish(ch, pin = pcPinSeen, fallbackId = pcId, fallbackName = pcName)
            }
        }
    }

    /** §4.4: issue a fresh token; store the PC only after the PC's answer. */
    private suspend fun finish(ch: PairingChannel, pin: String, fallbackId: String?, fallbackName: String) {
        _state.value = PairingState.Finishing(fallbackName)
        val token = Secrets.newToken()
        val result = ch.call("pair_finish", buildJsonObject { put("token", token) }, requestTimeout)
        val pc = result.lenientObj("pc")
        val id = pc?.lenientString("id")?.takeIf { it.isNotEmpty() } ?: fallbackId
            ?: throw ProtocolError("pair_finish result has no pc.id")
        val name = pc?.lenientString("name")?.takeIf { it.isNotBlank() } ?: fallbackName
        store(NewPairing(id, name, pin, token))
        _state.value = PairingState.Paired(id, name)
    }

    /**
     * Tries each host in order (§4.2). Network errors move on to the next host; so does a
     * pin mismatch, since a stale address may now be another machine. If every host fails and
     * any of them showed the wrong pin, that is what the user is told.
     */
    private suspend fun connectAny(pcName: String?, hosts: List<String>, port: Int, expectedPin: String?): PairingChannel {
        _state.value = PairingState.Connecting(pcName)
        var mismatch = false
        var lastError: Exception? = null
        for (host in hosts) {
            try {
                val ch = connector.connect(host, port, expectedPin)
                if (expectedPin != null && !Pins.matches(ch.pcPin, expectedPin)) {
                    ch.close()
                    mismatch = true
                    continue
                }
                return ch
            } catch (e: PinMismatchException) {
                mismatch = true
            } catch (e: IOException) {
                lastError = e
            }
        }
        throw Failure(if (mismatch) PairingFailure.PinMismatch else PairingFailure.Unreachable(lastError?.message))
    }

    private suspend fun run(pcName: String?, block: suspend () -> Unit) {
        codeAnswer = null
        try {
            block()
        } catch (e: Failure) {
            _state.value = PairingState.Failed(e.failure, pcName)
        } catch (e: ProtocolError) {
            _state.value = PairingState.Failed(PairingFailure.Protocol(e.message ?: "protocol error"), pcName)
        } catch (e: PairingRpcException) {
            _state.value = PairingState.Failed(failureFor(e), pcName)
        } catch (e: TimeoutCancellationException) {
            _state.value = PairingState.Failed(PairingFailure.TimedOut, pcName)
        } catch (e: CancellationException) {
            _state.value = PairingState.Idle
            throw e
        } catch (e: IOException) {
            _state.value = PairingState.Failed(PairingFailure.ConnectionLost(e.message), pcName)
        } catch (e: Exception) {
            // e.g. the phone's own key or settings couldn't be read: report, don't crash.
            _state.value = PairingState.Failed(PairingFailure.Protocol(e.message ?: e.javaClass.simpleName), pcName)
        } finally {
            codeAnswer = null
        }
    }

    private class Failure(val failure: PairingFailure) : Exception(failure.toString())
    private class ProtocolError(message: String) : Exception(message)

    companion object {
        private val SHORT = 3.seconds

        /** Default for the PC user's answers: at least 5 minutes (SECURITY.md §4.1). */
        val USER_TIMEOUT: Duration = 5.minutes + 30.seconds

        fun failureFor(e: PairingRpcException): PairingFailure = when (e.code) {
            PairingErrors.BAD_SECRET -> PairingFailure.BadSecret
            PairingErrors.REJECTED -> PairingFailure.Rejected
            PairingErrors.OUT_OF_ORDER -> PairingFailure.OutOfOrder
            PairingErrors.SAS_MISMATCH -> PairingFailure.CodesDiffer
            PairingErrors.EXPIRED -> PairingFailure.Expired
            PairingErrors.UNSUPPORTED_PROTOCOL -> PairingFailure.UnsupportedProtocol
            else -> PairingFailure.Protocol("${e.code}: ${e.message}")
        }
    }
}

internal fun PhoneInfo.toJson(): JsonObject = buildJsonObject {
    put("id", id)
    put("name", name)
    put("pin", pin)
    put("port", port)
    put("addresses", buildJsonArray { addresses.forEach { add(kotlinx.serialization.json.JsonPrimitive(it)) } })
}
