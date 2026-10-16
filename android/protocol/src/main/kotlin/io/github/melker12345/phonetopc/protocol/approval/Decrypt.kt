package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.invalidParams
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.string
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import java.security.KeyFactory
import java.security.interfaces.RSAPublicKey
import java.security.spec.MGF1ParameterSpec
import java.security.spec.X509EncodedKeySpec
import javax.crypto.spec.OAEPParameterSpec
import javax.crypto.spec.PSource

/** Decryption with the phone's key (DECRYPT.md). */
object DecryptCipher {
    /** DECRYPT.md §3. Android Keystore allows MGF1 with SHA-1 by default. */
    const val TRANSFORMATION = "RSA/ECB/OAEPWithSHA-256AndMGF1Padding"

    val OAEP: OAEPParameterSpec = OAEPParameterSpec("SHA-256", "MGF1", MGF1ParameterSpec.SHA1, PSource.PSpecified.DEFAULT)

    /** Ciphertext length for the key (base64url DER SPKI), or `null` if it isn't an RSA key. */
    fun ciphertextSize(key: ApprovalKeyInfo): Int? = try {
        val spki = Base64Url.decode(key.key) ?: return null
        val rsa = KeyFactory.getInstance("RSA").generatePublic(X509EncodedKeySpec(spki)) as RSAPublicKey
        (rsa.modulus.bitLength() + 7) / 8
    } catch (_: Exception) {
        null
    }
}

/** Validated `decrypt` params (DECRYPT.md §4). */
class DecryptParams(
    val reason: String,
    val program: String,
    val match: Int,
    val keyPin: String,
    val ciphertext: ByteArray,
    val timeoutMillis: Long,
) {
    companion object {
        /** @throws io.github.melker12345.phonetopc.protocol.RpcException `-32602`. */
        fun parse(params: JsonObject): DecryptParams {
            val reason = ApprovalParams.parseReason(params)
            val program = ApprovalParams.parseProgram(params)
            val match = ApprovalParams.parseMatch(params)
            val pin = params.string("key_pin") ?: invalidParams("key_pin is required")
            val text = params.string("ciphertext") ?: invalidParams("ciphertext is required")
            val ciphertext = Base64Url.decode(text)?.takeIf { it.isNotEmpty() } ?: invalidParams("ciphertext must be base64url")
            return DecryptParams(reason, program, match, pin, ciphertext, ApprovalParams.parseTimeout(params))
        }
    }
}

/** One decryption as the UI sees it, with number matching as in [ApprovalRequest]. */
class DecryptRequest(
    val id: Long,
    val pc: PcInfo,
    val program: String,
    val reason: String,
    val match: Int,
    val choices: List<Int>,
    val timeoutMillis: Long,
    val ciphertext: ByteArray,
)

sealed interface DecryptOutcome {
    class Decrypted(val plaintext: ByteArray) : DecryptOutcome

    data class Refused(val why: ApprovalWhy) : DecryptOutcome

    fun toJson(): JsonObject = buildJsonObject {
        when (this@DecryptOutcome) {
            is Decrypted -> {
                put("decrypted", true)
                put("plaintext", Base64Url.encode(plaintext))
            }
            is Refused -> {
                put("decrypted", false)
                put("why", why.wire)
            }
        }
    }
}
