package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.RpcError
import io.github.melker12345.phonetopc.protocol.RpcException
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.protocol.security.sha256
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.JsonElement
import kotlinx.serialization.json.JsonNull
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import kotlinx.serialization.json.buildJsonObject
import kotlinx.serialization.json.put
import java.security.KeyFactory
import java.security.Signature
import java.security.spec.X509EncodedKeySpec

/**
 * The approval public key as `hello.approval` reports it (APPROVAL.md §1): [key] is base64url
 * of the DER SubjectPublicKeyInfo, [pin] the SPKI pin over it (SECURITY.md §2).
 */
@Serializable
data class ApprovalKeyInfo(val key: String, val pin: String) {
    companion object {
        fun of(spkiDer: ByteArray) = ApprovalKeyInfo(Base64Url.encode(spkiDer), Pins.ofSpki(spkiDer))
    }
}

/** `why` of a refused approval (APPROVAL.md §4.1). */
enum class ApprovalWhy(val wire: String) {
    DENIED("denied"),
    CANCELLED("cancelled"),
    TIMEOUT("timeout"),
    BUSY("busy"),
    LOCKOUT("lockout"),
    UNAVAILABLE("unavailable"),
}

sealed interface ApprovalOutcome {
    /** The user passed the biometric check and the key signed [ApprovalRequest.message]. */
    class Approved(val signature: ByteArray) : ApprovalOutcome

    data class Refused(val why: ApprovalWhy) : ApprovalOutcome

    /** The `request_approval` result. */
    fun toJson(): JsonObject = buildJsonObject {
        when (this@ApprovalOutcome) {
            is Approved -> {
                put("approved", true)
                put("signature", Base64Url.encode(signature))
            }
            is Refused -> {
                put("approved", false)
                put("why", why.wire)
            }
        }
    }
}

/**
 * One approval as the UI sees it. [reason] is shown verbatim; [message] are the exact bytes
 * the approval key must sign (APPROVAL.md §5).
 */
class ApprovalRequest(
    val id: Long,
    val pc: PcInfo,
    val reason: String,
    val nonce: String,
    val timeoutMillis: Long,
    val message: ByteArray,
)

/**
 * The platform side of approvals: the Keystore key and the biometric prompt on Android, a fake
 * in tests.
 */
interface Approver {
    /**
     * The approval key, or `null` if approvals aren't possible right now (no strong biometric
     * enrolled, no key). Called for every secure `hello`; keep it cheap.
     */
    fun keyInfo(): ApprovalKeyInfo?

    /**
     * Asks the user and, on success, signs [ApprovalRequest.message] with the approval key.
     * Suspends until the user acted. The caller enforces the timeout and cancels this when the
     * request ends for any other reason (session closed); on cancellation every notification
     * and prompt for it must go away.
     */
    suspend fun approve(request: ApprovalRequest): ApprovalOutcome
}

/** The signed message of APPROVAL.md §5, and its verification. */
object ApprovalMessage {
    const val LABEL = "phonetopc-approval/1"

    /**
     * `phonetopc-approval/1 \n phone id \n pc id \n nonce \n base64url(SHA-256(reason))`, UTF-8,
     * no trailing newline. [nonce] is the base64url text exactly as the PC sent it.
     */
    fun build(phoneId: String, pcId: String, nonce: String, reason: String): ByteArray =
        listOf(LABEL, phoneId, pcId, nonce, Base64Url.encode(sha256(reason.toByteArray(Charsets.UTF_8))))
            .joinToString("\n")
            .toByteArray(Charsets.UTF_8)

    /**
     * ECDSA P-256 / SHA-256 over [message] with [key] (base64url DER SPKI); [signature] is
     * base64url DER. Anything malformed is simply `false`.
     */
    fun verify(key: String, message: ByteArray, signature: String): Boolean {
        val sig = Base64Url.decode(signature) ?: return false
        return verify(Base64Url.decode(key) ?: return false, message, sig)
    }

    fun verify(spkiDer: ByteArray, message: ByteArray, signature: ByteArray): Boolean = try {
        val publicKey = KeyFactory.getInstance("EC").generatePublic(X509EncodedKeySpec(spkiDer))
        Signature.getInstance(ALGORITHM).run {
            initVerify(publicKey)
            update(message)
            verify(signature)
        }
    } catch (_: Exception) {
        false
    }

    const val ALGORITHM = "SHA256withECDSA"
}

/** Validated `request_approval` params (APPROVAL.md §4). */
data class ApprovalParams(val reason: String, val nonce: String, val timeoutMillis: Long) {
    companion object {
        const val DEFAULT_TIMEOUT_MS = 30_000L
        const val MIN_TIMEOUT_MS = 5_000L
        const val MAX_TIMEOUT_MS = 120_000L
        const val MAX_REASON = 200
        const val NONCE_BYTES = 32

        /** @throws RpcException `-32602` for anything that breaks the rules of §4. */
        fun parse(params: JsonObject): ApprovalParams {
            // Accepted or rejected exactly as received: never trimmed or normalized, because the
            // PC hashes the string it sent.
            val reason = params.string("reason") ?: invalid("reason is required")
            if (!isDisplayable(reason)) {
                invalid("reason must not contain control characters, bidirectional controls or unpaired surrogates")
            }
            val length = reason.codePointCount(0, reason.length)
            if (length < 1) invalid("reason must not be empty")
            if (length > MAX_REASON) invalid("reason must be at most $MAX_REASON code points")
            if (isSpace(reason.codePointAt(0)) || isSpace(reason.codePointBefore(reason.length))) {
                invalid("reason must not start or end with whitespace")
            }

            val nonce = params.string("nonce") ?: invalid("nonce is required")
            if (Base64Url.decode(nonce)?.size != NONCE_BYTES) invalid("nonce must be $NONCE_BYTES bytes, base64url")

            val timeout = when (val t = params["timeout_ms"]) {
                null, JsonNull -> DEFAULT_TIMEOUT_MS
                else -> integer(t) ?: invalid("timeout_ms must be an integer")
            }
            if (timeout !in MIN_TIMEOUT_MS..MAX_TIMEOUT_MS) {
                invalid("timeout_ms must be between $MIN_TIMEOUT_MS and $MAX_TIMEOUT_MS")
            }
            return ApprovalParams(reason, nonce, timeout)
        }

        /**
         * No control characters (Unicode Cc: C0, DEL, C1, so no newlines or tabs either), no
         * bidirectional controls, which could make a reason look like another one, and no
         * unpaired surrogates, which have no UTF-8 encoding and so no well-defined signed hash.
         */
        fun isDisplayable(text: String): Boolean {
            var i = 0
            while (i < text.length) {
                val c = text[i]
                if (Character.isHighSurrogate(c)) {
                    if (i + 1 >= text.length || !Character.isLowSurrogate(text[i + 1])) return false
                    i += 2
                    continue
                }
                if (Character.isLowSurrogate(c) || Character.getType(c) == Character.CONTROL.toInt()) return false
                if (isBidiControl(c.code)) return false
                i++
            }
            return true
        }

        /** U+061C, U+200E, U+200F, U+202A–U+202E, U+2066–U+2069 (APPROVAL.md §4). */
        fun isBidiControl(cp: Int): Boolean =
            cp == 0x061C || cp == 0x200E || cp == 0x200F || cp in 0x202A..0x202E || cp in 0x2066..0x2069

        /**
         * Unicode White_Space: Java's whitespace plus the no-break spaces (U+00A0, U+2007,
         * U+202F). U+0085, the only other one, is a control character and rejected anyway.
         */
        private fun isSpace(cp: Int) = Character.isWhitespace(cp) || Character.isSpaceChar(cp)

        private fun JsonObject.string(key: String): String? = when (val v = this[key]) {
            null, JsonNull -> null
            is JsonPrimitive -> if (v.isString) v.content else invalid("$key must be a string")
            else -> invalid("$key must be a string")
        }

        /** A JSON integer spelled as one (`30000`, not `3e4` or `30000.0`). */
        private fun integer(e: JsonElement): Long? {
            val p = e as? JsonPrimitive ?: return null
            if (p.isString || !INTEGER.matches(p.content)) return null
            return p.content.toLongOrNull()
        }

        private val INTEGER = Regex("-?(0|[1-9][0-9]{0,17})")

        private fun invalid(message: String): Nothing = throw RpcException(RpcError.INVALID_PARAMS, message)
    }
}
