package io.github.melker12345.phonetopc.data

import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyProperties
import android.util.Log
import io.github.melker12345.phonetopc.server.TlsIdentity
import java.math.BigInteger
import java.security.KeyFactory
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.SecureRandom
import java.security.cert.X509Certificate
import java.security.spec.ECGenParameterSpec
import java.util.Calendar
import javax.security.auth.x500.X500Principal

/**
 * The phone's long-term TLS identity (SECURITY.md §2): an ECDSA P-256 key generated on first
 * use inside Android Keystore (hardware-backed where the phone has a TEE or StrongBox), with
 * the self-signed certificate Keystore makes for it. The private key is never exportable; TLS
 * signs with it through Keystore.
 *
 * The key outlives app updates, so the pin PCs stored keeps working. Uninstalling the app
 * deletes it, and every PC then has to pair again.
 */
object PhoneKey {
    private const val ALIAS = "phonetopc-tls-v1"
    private const val TAG = "PhoneKey"

    @Volatile private var cached: TlsIdentity? = null

    /** Blocking (Keystore IPC, key generation); call off the main thread. */
    @Synchronized
    fun identity(): TlsIdentity {
        cached?.let { return it }
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        val existing = ks.getKey(ALIAS, null) as? PrivateKey
        val cert = ks.getCertificate(ALIAS) as? X509Certificate
        val identity = if (existing != null && cert != null) {
            TlsIdentity(existing, cert)
        } else {
            generate()
            TlsIdentity(ks.getKey(ALIAS, null) as PrivateKey, ks.getCertificate(ALIAS) as X509Certificate)
        }
        cached = identity
        return identity
    }

    private fun generate() {
        val notBefore = Calendar.getInstance().apply { add(Calendar.DAY_OF_YEAR, -1) }.time
        // Nobody checks expiry (§2); a long validity just keeps strict TLS stacks quiet.
        val notAfter = Calendar.getInstance().apply { add(Calendar.YEAR, 100) }.time
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            // TLS stacks sign digests they computed themselves, hence NONE.
            .setDigests(KeyProperties.DIGEST_NONE, KeyProperties.DIGEST_SHA256)
            .setCertificateSubject(X500Principal("CN=phonetopc"))
            .setCertificateSerialNumber(BigInteger(64, SecureRandom()).add(BigInteger.ONE))
            .setCertificateNotBefore(notBefore)
            .setCertificateNotAfter(notAfter)
            .build()
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
            initialize(spec)
            generateKeyPair()
        }
        Log.i(TAG, "generated the TLS key")
    }

    fun isHardwareBacked(key: PrivateKey): Boolean = runCatching {
        val info = KeyFactory.getInstance(key.algorithm, "AndroidKeyStore").getKeySpec(key, KeyInfo::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            info.securityLevel == KeyProperties.SECURITY_LEVEL_TRUSTED_ENVIRONMENT ||
                info.securityLevel == KeyProperties.SECURITY_LEVEL_STRONGBOX
        } else {
            @Suppress("DEPRECATION")
            info.isInsideSecureHardware
        }
    }.getOrDefault(false)
}
