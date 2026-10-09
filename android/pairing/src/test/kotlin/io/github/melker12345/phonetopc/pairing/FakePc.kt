package io.github.melker12345.phonetopc.pairing

import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.ConstantTime
import io.github.melker12345.phonetopc.protocol.security.Sas
import io.github.melker12345.phonetopc.protocol.security.Secrets
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.awaitCancellation
import kotlinx.coroutines.withTimeout
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.boolean
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.jsonPrimitive
import kotlinx.serialization.json.put
import java.io.IOException
import kotlin.time.Duration

/**
 * A PC pairing listener that follows SECURITY.md §4, in memory. Knobs let tests make it
 * misbehave or have its user say no.
 */
class FakePc(
    val id: String = "9d1c0a5e7b",
    val name: String = "arch-desktop",
    val pin: String = PC_PIN,
    val secret: String = Base64Url.encode(Secrets.bytes(16)),
    /** Hosts that accept connections; others throw like an unreachable address. */
    val reachable: Set<String> = setOf("192.168.1.10"),
    /** What the pairing listener presents (a MITM would show another pin). */
    var presentedPin: String = pin,
) : PairingConnector {
    var userApproves = true
    var userConfirmsCode = true
    var cheatCommitment = false
    var neverAnswerApproval = false
    var dropAfter: String? = null
    var errorFor: Pair<String, Int>? = null

    /** What the PC saw / stored. */
    val calls = mutableListOf<String>()
    var phoneSeen: JsonObject? = null
    var storedToken: String? = null
    var sasConfirmFromPhone: Boolean? = null
    var pcCode: String? = null

    private var step = "start"
    private var npc = ByteArray(0)
    private var nphone = ByteArray(0)
    val connects = mutableListOf<Pair<String, String?>>()

    override suspend fun connect(host: String, port: Int, expectedPin: String?): PairingChannel {
        connects += host to expectedPin
        if (host !in reachable) throw IOException("connect to $host:$port failed")
        if (expectedPin != null && expectedPin != presentedPin) throw PinMismatchException(expectedPin, presentedPin)
        return Channel()
    }

    private inner class Channel : PairingChannel {
        override val pcPin: String get() = presentedPin
        override fun close() {}

        override suspend fun call(method: String, params: JsonObject, timeout: Duration): JsonObject {
            calls += method
            if (dropAfter == method) throw IOException("connection reset")
            errorFor?.let { (m, code) -> if (m == method) throw PairingRpcException(code, "scripted") }
            return when (method) {
                "pair_qr" -> {
                    if (params.str("protocol") != PAIR_PROTOCOL) throw PairingRpcException(-32004, "unsupported")
                    if (!ConstantTime.equals(params.str("secret") ?: "", secret)) throw PairingRpcException(-32020, "bad secret")
                    phoneSeen = params["phone"] as JsonObject
                    if (neverAnswerApproval) {
                        // Like the real channel, the call itself enforces the timeout.
                        withTimeout(timeout) { CompletableDeferred<Unit>().await() }
                    }
                    if (!userApproves) throw PairingRpcException(-32021, "rejected")
                    step = "approved"
                    buildJsonObject {}
                }
                "pair_sas_begin" -> {
                    phoneSeen = params["phone"] as JsonObject
                    npc = Secrets.newNonce()
                    step = "committed"
                    val commitment = if (cheatCommitment) Sas.commitment(Secrets.newNonce()) else Sas.commitment(npc)
                    buildJsonObject {
                        put("pc", buildJsonObject { put("id", id); put("name", name) })
                        put("commitment", commitment)
                    }
                }
                "pair_sas_nonce" -> {
                    if (step != "committed") throw PairingRpcException(-32022, "out of order")
                    nphone = Base64Url.decode(params.str("nonce")!!)!!
                    // The PC computes the code with its own pin and the phone's claimed pin.
                    pcCode = Sas.code(pin, phoneSeen!!["pin"]!!.jsonPrimitive.content, npc, nphone)
                    step = "revealed"
                    buildJsonObject { put("nonce", Base64Url.encode(npc)) }
                }
                "pair_sas_confirm" -> {
                    val match = params["match"]!!.jsonPrimitive.boolean
                    sasConfirmFromPhone = match
                    if (!match || !userConfirmsCode) {
                        step = "dead"
                        throw PairingRpcException(-32023, "sas mismatch")
                    }
                    step = "approved"
                    buildJsonObject {}
                }
                "pair_finish" -> {
                    if (step != "approved") throw PairingRpcException(-32022, "out of order")
                    storedToken = params.str("token")
                    buildJsonObject { put("pc", buildJsonObject { put("id", id); put("name", name) }) }
                }
                else -> awaitCancellation()
            }
        }
    }

    fun qr(hosts: List<String> = reachable.toList(), qrSecret: String = secret, qrPin: String = pin) = PairingQr(
        pcId = id, pcName = name, hosts = hosts, port = 40123, pin = qrPin, secret = qrSecret,
    )

    companion object {
        const val PC_PIN = "sha256/89Nebgssmdcpo6zU3xFLFXCRqV4YpVYbvJbyq99cMGA"
        const val PHONE_PIN = "sha256/tQITurCV6l1Rq9Rk0X_kPrHlWaaKPnkI6CGl03GEaBc"
        const val MITM_PIN = "sha256/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    }
}

private fun JsonObject.str(key: String) = (this[key] as? JsonPrimitive)?.content
