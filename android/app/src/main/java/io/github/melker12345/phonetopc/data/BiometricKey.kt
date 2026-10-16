package io.github.melker12345.phonetopc.data

import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import android.os.SystemClock
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyInfo
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.security.keystore.StrongBoxUnavailableException
import android.util.Log
import androidx.annotation.RequiresApi
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import io.github.melker12345.phonetopc.protocol.approval.ApprovalKeyInfo
import io.github.melker12345.phonetopc.protocol.approval.KeyAttestation
import java.security.KeyFactory
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey

/**
 * A key pair in Android Keystore that works only right after a strong (Class 3) biometric
 * check, once per check, and that Android invalidates when fingerprints are added or removed.
 * StrongBox where the phone has it. Used through `BiometricPrompt` with a `CryptoObject` around
 * the operation [forPrompt] initialises; nothing else can use the key.
 *
 * Keystore attests it (APPROVAL.md §3.1) with a challenge that ties it to the phone's TLS key.
 * Keys under [oldAliases] were made without attestation; they are deleted.
 */
abstract class BiometricKey(private val alias: String, private val oldAliases: List<String>, private val tag: String) {
    /** The algorithm for `KeyPairGenerator` and `KeyFactory`. */
    protected abstract val algorithm: String

    /** The key's generation spec: purpose, size, padding or digest. Authentication is added. */
    protected abstract fun spec(alias: String, strongBox: Boolean): KeyGenParameterSpec.Builder

    /** Initialises the operation that uses [key]; for a per-use key this needs no authentication. */
    protected abstract fun init(key: PrivateKey): Any

    enum class Problem {
        /** No strong (Class 3) biometric sensor. */
        NO_HARDWARE,

        NONE_ENROLLED,

        HW_UNAVAILABLE,

        SECURITY_UPDATE_REQUIRED,

        /** Anything else, including a key that couldn't be created. */
        OTHER,
    }

    sealed interface Status {
        data class Available(val info: ApprovalKeyInfo, val strongBox: Boolean) : Status
        data class Unavailable(val problem: Problem) : Status
    }

    class KeyReplacedException : Exception("the key was invalidated and replaced")

    @Volatile private var cached: Pair<Long, Status>? = null

    /**
     * Whether the key can be used, and its public part. Creates the key the first time, and
     * replaces it if Android invalidated it. Blocking (Keystore, maybe key generation); cached
     * for a few seconds, so it's cheap enough for every `hello`.
     */
    @Synchronized
    fun status(context: Context, fresh: Boolean = false): Status {
        val now = SystemClock.elapsedRealtime()
        cached?.let { (at, status) -> if (!fresh && now - at < CACHE_MILLIS) return status }
        val status = compute(context)
        cached = now to status
        return status
    }

    private fun compute(context: Context): Status {
        when (BiometricManager.from(context).canAuthenticate(BIOMETRIC_STRONG)) {
            BiometricManager.BIOMETRIC_SUCCESS -> Unit
            BiometricManager.BIOMETRIC_ERROR_NO_HARDWARE -> return Status.Unavailable(Problem.NO_HARDWARE)
            BiometricManager.BIOMETRIC_ERROR_NONE_ENROLLED -> return Status.Unavailable(Problem.NONE_ENROLLED)
            BiometricManager.BIOMETRIC_ERROR_HW_UNAVAILABLE -> return Status.Unavailable(Problem.HW_UNAVAILABLE)
            BiometricManager.BIOMETRIC_ERROR_SECURITY_UPDATE_REQUIRED ->
                return Status.Unavailable(Problem.SECURITY_UPDATE_REQUIRED)
            else -> return Status.Unavailable(Problem.OTHER)
        }
        return try {
            val ks = keyStore()
            for (old in oldAliases) if (ks.containsAlias(old)) ks.deleteEntry(old)
            val key = validKey(ks) ?: run {
                generate(context)
                ks.getKey(alias, null) as PrivateKey
            }
            val chain = ks.getCertificateChain(alias)
            val spki = chain.first().publicKey.encoded
            // A chain of one is the self-signed certificate of a key without attestation.
            val attestation = if (chain.size > 1) chain.map { it.encoded } else emptyList()
            Status.Available(ApprovalKeyInfo.of(spki, attestation), isStrongBox(key))
        } catch (e: Exception) {
            Log.w(tag, "key unavailable", e)
            Status.Unavailable(Problem.OTHER)
        }
    }

    /**
     * The operation, ready for `BiometricPrompt.CryptoObject`. Initialising it is also how an
     * invalidated key shows: then the key is deleted and recreated and [KeyReplacedException]
     * is thrown, since the PC only knows the old key.
     */
    @Synchronized
    protected fun forPrompt(context: Context): Any {
        val ks = keyStore()
        val key = ks.getKey(alias, null) as? PrivateKey ?: throw KeyReplacedException().also {
            cached = null
            status(context, fresh = true)
        }
        return try {
            init(key)
        } catch (e: KeyPermanentlyInvalidatedException) {
            Log.i(tag, "key invalidated (biometric enrollment changed); replacing it")
            ks.deleteEntry(alias)
            cached = null
            status(context, fresh = true)
            throw KeyReplacedException()
        }
    }

    /** The current key, or `null` if there is none or Android invalidated it (then it's deleted). */
    private fun validKey(ks: KeyStore): PrivateKey? {
        val key = ks.getKey(alias, null) as? PrivateKey ?: return null
        return try {
            init(key)
            key
        } catch (e: KeyPermanentlyInvalidatedException) {
            Log.i(tag, "key invalidated (biometric enrollment changed); replacing it")
            ks.deleteEntry(alias)
            null
        }
    }

    private fun generate(context: Context) {
        val challenge = KeyAttestation.challenge(PhoneKey.identity().certificate.publicKey.encoded)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P &&
            context.packageManager.hasSystemFeature(PackageManager.FEATURE_STRONGBOX_KEYSTORE) &&
            generateInStrongBox(challenge)
        ) {
            return
        }
        generate(strongBox = false, challenge)
    }

    /** `false` if StrongBox refused (e.g. it's busy or lacks the algorithm): use the TEE then. */
    @RequiresApi(Build.VERSION_CODES.P)
    private fun generateInStrongBox(challenge: ByteArray): Boolean = try {
        generate(strongBox = true, challenge)
        true
    } catch (e: StrongBoxUnavailableException) {
        Log.i(tag, "StrongBox unavailable; using the TEE", e)
        false
    }

    @Suppress("DEPRECATION") // setUserAuthenticationValidityDurationSeconds: the API 26-29 way
    private fun generate(strongBox: Boolean, challenge: ByteArray) {
        val spec = spec(alias, strongBox)
            .setUserAuthenticationRequired(true)
            .setInvalidatedByBiometricEnrollment(true)
            .setAttestationChallenge(challenge)
            .apply {
                if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                    // Every use needs its own strong biometric check; no PIN/pattern fallback.
                    setUserAuthenticationParameters(0, KeyProperties.AUTH_BIOMETRIC_STRONG)
                } else {
                    // -1: authorise each operation with a fingerprint (CryptoObject) only.
                    setUserAuthenticationValidityDurationSeconds(-1)
                }
                if (strongBox && Build.VERSION.SDK_INT >= Build.VERSION_CODES.P) setIsStrongBoxBacked(true)
            }
            .build()
        KeyPairGenerator.getInstance(algorithm, "AndroidKeyStore").apply {
            initialize(spec)
            generateKeyPair()
        }
        Log.i(tag, "generated the key (StrongBox: $strongBox)")
    }

    private fun isStrongBox(key: PrivateKey): Boolean = runCatching {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) return@runCatching false
        val info = KeyFactory.getInstance(key.algorithm, "AndroidKeyStore").getKeySpec(key, KeyInfo::class.java)
        info.securityLevel == KeyProperties.SECURITY_LEVEL_STRONGBOX
    }.getOrDefault(false)

    private fun keyStore() = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }

    private companion object {
        /** How long [status] is reused; the check itself starts a Keystore operation. */
        const val CACHE_MILLIS = 10_000L
    }
}
