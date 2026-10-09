package io.github.melker12345.phonetopc.protocol.security

import java.security.KeyFactory
import java.security.PrivateKey
import java.security.spec.PKCS8EncodedKeySpec
import java.util.Base64

/** Minimal PEM reading for EC P-256 keys (tests and tooling; the phone's own key lives in Keystore). */
object Pem {
    fun body(pem: String, label: String): ByteArray {
        val begin = "-----BEGIN $label-----"
        val end = "-----END $label-----"
        val start = pem.indexOf(begin)
        val stop = pem.indexOf(end)
        require(start >= 0 && stop > start) { "no $label block" }
        return Base64.getMimeDecoder().decode(pem.substring(start + begin.length, stop))
    }

    /** Reads `EC PRIVATE KEY` (SEC 1, P-256) or `PRIVATE KEY` (PKCS #8). */
    fun ecPrivateKey(pem: String): PrivateKey {
        val pkcs8 = if (pem.contains("BEGIN EC PRIVATE KEY")) sec1ToPkcs8(body(pem, "EC PRIVATE KEY")) else body(pem, "PRIVATE KEY")
        return KeyFactory.getInstance("EC").generatePrivate(PKCS8EncodedKeySpec(pkcs8))
    }

    /**
     * Wraps a SEC 1 ECPrivateKey in PKCS #8: SEQUENCE { 0, AlgorithmIdentifier(ecPublicKey,
     * prime256v1), OCTET STRING(sec1) }. Only for short-form lengths, which P-256 keys have.
     */
    private fun sec1ToPkcs8(sec1: ByteArray): ByteArray {
        require(sec1.size < 128) { "unexpected SEC1 key size" }
        val algId = byteArrayOf(
            0x30, 0x13,
            0x06, 0x07, 0x2a, 0x86.toByte(), 0x48, 0xce.toByte(), 0x3d, 0x02, 0x01, // 1.2.840.10045.2.1
            0x06, 0x08, 0x2a, 0x86.toByte(), 0x48, 0xce.toByte(), 0x3d, 0x03, 0x01, 0x07, // 1.2.840.10045.3.1.7
        )
        val version = byteArrayOf(0x02, 0x01, 0x00)
        val octet = byteArrayOf(0x04, sec1.size.toByte()) + sec1
        val content = version + algId + octet
        require(content.size < 256)
        return byteArrayOf(0x30, 0x81.toByte(), content.size.toByte()) + content
    }
}
