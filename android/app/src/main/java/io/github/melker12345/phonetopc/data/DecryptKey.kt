package io.github.melker12345.phonetopc.data

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import io.github.melker12345.phonetopc.protocol.approval.DecryptCipher
import java.math.BigInteger
import java.security.PrivateKey
import java.security.spec.RSAKeyGenParameterSpec
import javax.crypto.Cipher

/**
 * The decryption key (DECRYPT.md §2): RSA with OAEP, separate from the TLS and approval keys.
 * 2048 bits in StrongBox, which need not support more; 3072 in the TEE.
 */
object DecryptKey : BiometricKey("phonetopc-decrypt-v2", listOf("phonetopc-decrypt-v1"), "DecryptKey") {
    override val algorithm = KeyProperties.KEY_ALGORITHM_RSA

    override fun spec(alias: String, strongBox: Boolean): KeyGenParameterSpec.Builder =
        KeyGenParameterSpec.Builder(alias, KeyProperties.PURPOSE_DECRYPT)
            .setAlgorithmParameterSpec(RSAKeyGenParameterSpec(if (strongBox) 2048 else 3072, BigInteger.valueOf(65537)))
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_RSA_OAEP)
            .setDigests(KeyProperties.DIGEST_SHA256)

    override fun init(key: PrivateKey): Cipher =
        Cipher.getInstance(DecryptCipher.TRANSFORMATION).apply { init(Cipher.DECRYPT_MODE, key, DecryptCipher.OAEP) }

    /** A `Cipher` for `BiometricPrompt.CryptoObject`; see [forPrompt]. */
    fun cipherForPrompt(context: Context): Cipher = forPrompt(context) as Cipher
}
