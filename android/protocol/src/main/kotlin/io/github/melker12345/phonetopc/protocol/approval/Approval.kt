package io.github.melker12345.phonetopc.protocol.approval

import io.github.melker12345.phonetopc.protocol.PcInfo
import io.github.melker12345.phonetopc.protocol.RpcException
import io.github.melker12345.phonetopc.protocol.invalidParams
import io.github.melker12345.phonetopc.protocol.isHidden
import io.github.melker12345.phonetopc.protocol.security.Base64Url
import io.github.melker12345.phonetopc.protocol.security.Pins
import io.github.melker12345.phonetopc.protocol.security.sha256
import io.github.melker12345.phonetopc.protocol.string
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
 * of the DER SubjectPublicKeyInfo, [pin] the SPKI pin over it (SECURITY.md §2), and
 * [attestation] its Keystore attestation chain (§3.1), base64url DER, leaf first; omitted
 * when there is none.
 */
@Serializable
data class ApprovalKeyInfo(val key: String, val pin: String, val attestation: List<String>? = null) {
    companion object {
        fun of(spkiDer: ByteArray, chain: List<ByteArray> = emptyList()) = ApprovalKeyInfo(
            Base64Url.encode(spkiDer),
            Pins.ofSpki(spkiDer),
            chain.map { Base64Url.encode(it) }.ifEmpty { null },
        )
    }
}

/** Key attestation (APPROVAL.md §3.1). */
object KeyAttestation {
    /**
     * The attestation challenge of the phone's approval and decryption keys: SHA-256 of its TLS
     * SubjectPublicKeyInfo, which ties them to the key that PCs pin.
     */
    fun challenge(tlsSpkiDer: ByteArray): ByteArray = sha256(tlsSpkiDer)
}

/** `why` of a refused approval (APPROVAL.md §4.1). */
enum class ApprovalWhy(val wire: String) {
    DENIED("denied"),
    CANCELLED("cancelled"),
    TIMEOUT("timeout"),
    BUSY("busy"),
    LOCKOUT("lockout"),
    UNAVAILABLE("unavailable"),

    /** `decrypt` only: the user confirmed, but the ciphertext doesn't decrypt (DECRYPT.md §4). */
    FAILED("failed"),
}

sealed interface ApprovalOutcome {
    /** The user passed the biometric check and the key signed [ApprovalRequest.message]. */
    class Approved(val signature: ByteArray) : ApprovalOutcome

    data class Refused(val why: ApprovalWhy) : ApprovalOutcome

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

    /** The decryption key (DECRYPT.md §1), like [keyInfo]; `null` if decrypting isn't possible. */
    fun decryptKeyInfo(): ApprovalKeyInfo? = null

    /**
     * Asks the user and, on success, decrypts [DecryptRequest.ciphertext] with the decryption
     * key. Cancellation as for [approve].
     */
    suspend fun decrypt(request: DecryptRequest): DecryptOutcome = DecryptOutcome.Refused(ApprovalWhy.UNAVAILABLE)
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

    /** [key] (SPKI) and [signature] are base64url DER; anything malformed is `false`. */
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
            val reason = parseReason(params)
            val nonce = params.string("nonce") ?: invalidParams("nonce is required")
            if (Base64Url.decode(nonce)?.size != NONCE_BYTES) invalidParams("nonce must be $NONCE_BYTES bytes, base64url")
            return ApprovalParams(reason, nonce, parseTimeout(params))
        }

        /** `reason` of `request_approval` and `decrypt`. */
        internal fun parseReason(params: JsonObject): String {
            // Accepted or rejected exactly as received: never trimmed or normalized, because the
            // PC hashes the string it sent.
            val reason = params.string("reason") ?: invalidParams("reason is required")
            if (!isDisplayable(reason)) {
                invalidParams("reason must not contain control characters, bidirectional controls, invisible characters, line separators or unpaired surrogates")
            }
            val length = reason.codePointCount(0, reason.length)
            if (length < 1) invalidParams("reason must not be empty")
            if (length > MAX_REASON) invalidParams("reason must be at most $MAX_REASON code points")
            if (isSpace(reason.codePointAt(0)) || isSpace(reason.codePointBefore(reason.length))) {
                invalidParams("reason must not start or end with whitespace")
            }
            return reason
        }

        /** `timeout_ms` of `request_approval` and `decrypt`. */
        internal fun parseTimeout(params: JsonObject): Long {
            val timeout = when (val t = params["timeout_ms"]) {
                null, JsonNull -> DEFAULT_TIMEOUT_MS
                else -> integer(t) ?: invalidParams("timeout_ms must be an integer")
            }
            if (timeout !in MIN_TIMEOUT_MS..MAX_TIMEOUT_MS) {
                invalidParams("timeout_ms must be between $MIN_TIMEOUT_MS and $MAX_TIMEOUT_MS")
            }
            return timeout
        }

        /**
         * No control characters (Unicode Cc: C0, DEL, C1, so no newlines or tabs either), nothing
         * [isHidden], which could make a reason look like another one, and no unpaired
         * surrogates, which have no UTF-8 encoding and so no well-defined signed hash.
         */
        fun isDisplayable(text: String): Boolean {
            var i = 0
            while (i < text.length) {
                val c = text[i]
                if (Character.isLowSurrogate(c)) return false
                if (Character.isHighSurrogate(c) && (i + 1 >= text.length || !Character.isLowSurrogate(text[i + 1]))) return false
                val cp = text.codePointAt(i)
                if (Character.getType(cp) == Character.CONTROL.toInt() || isHidden(cp)) return false
                i += Character.charCount(cp)
            }
            return true
        }

        /**
         * Unicode White_Space: Java's whitespace plus the no-break spaces (U+00A0, U+2007,
         * U+202F). U+0085, the only other one, is a control character and rejected anyway.
         */
        private fun isSpace(cp: Int) = Character.isWhitespace(cp) || Character.isSpaceChar(cp)

        /** A JSON integer spelled as one (`30000`, not `3e4` or `30000.0`). */
        private fun integer(e: JsonElement): Long? {
            val p = e as? JsonPrimitive ?: return null
            if (p.isString || !INTEGER.matches(p.content)) return null
            return p.content.toLongOrNull()
        }

        private val INTEGER = Regex("-?(0|[1-9][0-9]{0,17})")
    }
}
