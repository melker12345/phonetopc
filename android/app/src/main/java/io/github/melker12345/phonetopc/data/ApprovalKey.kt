package io.github.melker12345.phonetopc.data

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import io.github.melker12345.phonetopc.protocol.approval.ApprovalMessage
import java.security.PrivateKey
import java.security.Signature
import java.security.spec.ECGenParameterSpec

/** The approval key (APPROVAL.md §2): ECDSA P-256, separate from the TLS key. */
object ApprovalKey : BiometricKey("phonetopc-approval-v1", "ApprovalKey") {
    override val algorithm = KeyProperties.KEY_ALGORITHM_EC

    override fun spec(alias: String, strongBox: Boolean): KeyGenParameterSpec.Builder =
        KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_SIGN)
            .setAlgorithmParameterSpec(ECGenParameterSpec("secp256r1"))
            .setDigests(KeyProperties.DIGEST_SHA256)

    override fun init(key: PrivateKey): Signature = Signature.getInstance(ApprovalMessage.ALGORITHM).apply { initSign(key) }

    /** A `Signature` for `BiometricPrompt.CryptoObject`; see [forPrompt]. */
    fun signatureForPrompt(context: Context): Signature = forPrompt(context) as Signature
}
