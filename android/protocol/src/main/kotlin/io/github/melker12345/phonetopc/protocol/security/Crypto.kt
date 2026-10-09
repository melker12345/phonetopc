package io.github.melker12345.phonetopc.protocol.security

import java.security.MessageDigest
import java.security.SecureRandom
import java.security.cert.CertificateFactory
import java.security.cert.X509Certificate
import java.util.Base64

/** base64url without padding (RFC 4648 §5), as used for pins, tokens, nonces and secrets (SECURITY.md). */
object Base64Url {
    private val encoder = Base64.getUrlEncoder().withoutPadding()
    private val decoder = Base64.getUrlDecoder()

    fun encode(bytes: ByteArray): String = encoder.encodeToString(bytes)

    /**
     * Strict decode: only the URL-safe alphabet and no padding. Returns `null` for anything
     * else, so `+`, `/`, `=` and whitespace are rejected rather than silently tolerated.
     */
    fun decode(text: String): ByteArray? {
        if (!ALPHABET.matches(text) || text.length % 4 == 1) return null
        return try {
            decoder.decode(text)
        } catch (_: IllegalArgumentException) {
            null
        }
    }

    private val ALPHABET = Regex("[A-Za-z0-9_-]*")
}

fun sha256(vararg parts: ByteArray): ByteArray {
    val md = MessageDigest.getInstance("SHA-256")
    parts.forEach { md.update(it) }
    return md.digest()
}

/** SPKI pins (SECURITY.md §2): `sha256/` + base64url(SHA-256(DER SubjectPublicKeyInfo)). */
object Pins {
    const val PREFIX = "sha256/"

    fun ofSpki(spkiDer: ByteArray): String = PREFIX + Base64Url.encode(sha256(spkiDer))

    /** `X509Certificate.publicKey.encoded` is the DER SubjectPublicKeyInfo. */
    fun of(cert: X509Certificate): String = ofSpki(cert.publicKey.encoded)

    /** A well-formed pin: the prefix and 32 bytes of base64url. */
    fun isValid(pin: String): Boolean =
        pin.startsWith(PREFIX) && Base64Url.decode(pin.removePrefix(PREFIX))?.size == 32

    /** Pins are public, but compare them in constant time anyway; it costs nothing. */
    fun matches(a: String, b: String): Boolean = ConstantTime.equals(a, b)

    /**
     * Short, human-friendly rendering for manual comparison: the first [bytes] bytes of the
     * hash as upper-case hex in groups of two bytes, e.g. `F38D 5E6E 0B2C 99D7`.
     */
    fun shortHex(pin: String, bytes: Int = 8): String {
        val raw = Base64Url.decode(pin.removePrefix(PREFIX)) ?: return pin
        return raw.take(bytes).chunked(2).joinToString(" ") { group -> group.joinToString("") { "%02X".format(it) } }
    }

    fun parsePemCertificate(pem: String): X509Certificate =
        CertificateFactory.getInstance("X.509").generateCertificate(pem.byteInputStream()) as X509Certificate
}

object ConstantTime {
    /** Constant-time for equal-length inputs; lengths are not secret here. */
    fun equals(a: ByteArray, b: ByteArray): Boolean = MessageDigest.isEqual(a, b)

    fun equals(a: String, b: String): Boolean = equals(a.toByteArray(Charsets.UTF_8), b.toByteArray(Charsets.UTF_8))
}

/** Every secret and nonce comes from here (SECURITY.md §1). */
object Secrets {
    private val random = SecureRandom()

    fun bytes(n: Int): ByteArray = ByteArray(n).also { random.nextBytes(it) }

    /** A new token for a PC: 32 random bytes, base64url (§4.4). */
    fun newToken(): String = Base64Url.encode(bytes(TOKEN_BYTES))

    /** A SAS nonce `Nphone`: 32 random bytes (§4.3). */
    fun newNonce(): ByteArray = bytes(NONCE_BYTES)

    const val TOKEN_BYTES = 32
    const val NONCE_BYTES = 32
}

/**
 * Tokens as the phone stores them: SHA-256 of the token's ASCII text, base64url. The phone
 * keeps only this (SECURITY.md §2 allows it), so a leaked settings file doesn't reveal tokens.
 */
object TokenHash {
    fun of(token: String): String = Base64Url.encode(sha256(token.toByteArray(Charsets.US_ASCII)))

    fun matches(token: String, storedHash: String): Boolean = ConstantTime.equals(of(token), storedHash)
}

/** The committed numeric comparison of SECURITY.md §4.3. */
object Sas {
    private val LABEL = "phonetopc-sas/1".toByteArray(Charsets.US_ASCII)

    /** `commitment` = base64url(SHA-256(Npc)). */
    fun commitment(npc: ByteArray): String = Base64Url.encode(sha256(npc))

    /** Checks the PC's revealed nonce against its earlier commitment, in constant time. */
    fun verifyCommitment(commitment: String, npc: ByteArray): Boolean =
        ConstantTime.equals(commitment(npc), commitment)

    /** SHA-256("phonetopc-sas/1" ‖ pc_pin ‖ phone_pin ‖ Npc ‖ Nphone). */
    fun digest(pcPin: String, phonePin: String, npc: ByteArray, nphone: ByteArray): ByteArray =
        sha256(LABEL, pcPin.toByteArray(Charsets.US_ASCII), phonePin.toByteArray(Charsets.US_ASCII), npc, nphone)

    /** The 6-digit code: first 4 bytes of [digest] as big-endian uint32, mod 1 000 000, zero-padded. */
    fun code(pcPin: String, phonePin: String, npc: ByteArray, nphone: ByteArray): String {
        val d = digest(pcPin, phonePin, npc, nphone)
        val v = ((d[0].toLong() and 0xff) shl 24) or ((d[1].toLong() and 0xff) shl 16) or
            ((d[2].toLong() and 0xff) shl 8) or (d[3].toLong() and 0xff)
        return "%06d".format(v % 1_000_000)
    }

    /** `940411` → `940 411`, for display. */
    fun format(code: String): String = if (code.length == 6) "${code.substring(0, 3)} ${code.substring(3)}" else code
}
