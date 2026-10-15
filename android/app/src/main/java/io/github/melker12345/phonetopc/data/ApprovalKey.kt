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
import io.github.melker12345.phonetopc.protocol.approval.ApprovalMessage
import java.security.KeyFactory
import java.security.KeyPairGenerator
import java.security.KeyStore
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/**
 * The approval key (APPROVAL.md §2): an ECDSA P-256 key in Android Keystore, separate from the
 * TLS key, that can sign only right after a strong (Class 3) biometric check, once per check,
 * and that Android invalidates when fingerprints are added or removed. StrongBox where the
 * phone has it.
 *
 * Signing goes through `BiometricPrompt` with a `CryptoObject` around [signatureForPrompt]'s
 * `Signature`; nothing else can use the key.
 */
object ApprovalKey {
    private const val ALIAS = "phonetopc-approval-v1"
    private const val TAG = "ApprovalKey"

    /** How long [status] is reused; the check itself starts a Keystore operation. */
    private const val CACHE_MILLIS = 10_000L

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

    class KeyReplacedException : Exception("the approval key was invalidated and replaced")

    @Volatile private var cached: Pair<Long, Status>? = null

    /**
     * Whether approvals are possible, and the key's public part. Creates the key the first
     * time, and replaces it if Android invalidated it. Blocking (Keystore, maybe key
     * generation); cached for a few seconds, so it's cheap enough for every `hello`.
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
        // APPROVAL.md §1: a Class 3 biometric, at least one enrolled.
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
            val key = validKey(ks) ?: run {
                generate(context)
                ks.getKey(ALIAS, null) as PrivateKey
            }
            val spki = ks.getCertificate(ALIAS).publicKey.encoded
            Status.Available(ApprovalKeyInfo.of(spki), isStrongBox(key))
        } catch (e: Exception) {
            Log.w(TAG, "approval key unavailable", e)
            Status.Unavailable(Problem.OTHER)
        }
    }

    /**
     * A `Signature` ready for `BiometricPrompt.CryptoObject`. Initialising it is also how an
     * invalidated key shows: then the key is deleted and recreated (APPROVAL.md §2) and
     * [KeyReplacedException] is thrown, since the PC only knows the old key.
     */
    @Synchronized
    fun signatureForPrompt(context: Context): Signature {
        val ks = keyStore()
        val key = ks.getKey(ALIAS, null) as? PrivateKey ?: throw KeyReplacedException().also {
            cached = null
            status(context, fresh = true)
        }
        return try {
            Signature.getInstance(ApprovalMessage.ALGORITHM).apply { initSign(key) }
        } catch (e: KeyPermanentlyInvalidatedException) {
            Log.i(TAG, "approval key invalidated (biometric enrollment changed); replacing it")
            ks.deleteEntry(ALIAS)
            cached = null
            status(context, fresh = true)
            throw KeyReplacedException()
        }
    }

    /** The current key, or `null` if there is none or Android invalidated it (then it's deleted). */
    private fun validKey(ks: KeyStore): PrivateKey? {
        val key = ks.getKey(ALIAS, null) as? PrivateKey ?: return null
        return try {
            // For a per-use key this needs no authentication; it fails only if invalidated.
            Signature.getInstance(ApprovalMessage.ALGORITHM).initSign(key)
            key
        } catch (e: KeyPermanentlyInvalidatedException) {
            Log.i(TAG, "approval key invalidated (biometric enrollment changed); replacing it")
            ks.deleteEntry(ALIAS)
            null
        }
    }

    private fun generate(context: Context) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.P &&
            context.packageManager.hasSystemFeature(PackageManager.FEATURE_STRONGBOX_KEYSTORE) &&
            generateInStrongBox()
        ) {
            return
        }
        generate(strongBox = false)
    }

    /** `false` if StrongBox refused (e.g. it's busy or lacks the algorithm): use the TEE then. */
    @RequiresApi(Build.VERSION_CODES.P)
    private fun generateInStrongBox(): Boolean = try {
        generate(strongBox = true)
        true
    } catch (e: StrongBoxUnavailableException) {
        Log.i(TAG, "StrongBox unavailable for the approval key; using the TEE", e)
        false
    }

    @Suppress("DEPRECATION") // setUserAuthenticationValidityDurationSeconds: the API 26-29 way
    private fun generate(strongBox: Boolean) {
        val spec = KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)
            .setUserAuthenticationRequired(true)
            .setInvalidatedByBiometricEnrollment(true)
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
        KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, "AndroidKeyStore").apply {
            initialize(spec)
            generateKeyPair()
        }
        Log.i(TAG, "generated the approval key (StrongBox: $strongBox)")
    }

    private fun isStrongBox(key: PrivateKey): Boolean = runCatching {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) return@runCatching false
        val info = KeyFactory.getInstance(key.algorithm, "AndroidKeyStore").getKeySpec(key, KeyInfo::class.java)
        info.securityLevel == KeyProperties.SECURITY_LEVEL_STRONGBOX
    }.getOrDefault(false)

    private fun keyStore() = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
}
