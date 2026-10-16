package io.github.melker12345.phonetopc.approval

import android.content.Context
import android.content.Intent
import android.util.Log
import androidx.biometric.BiometricPrompt
import io.github.melker12345.phonetopc.data.ApprovalKey
import io.github.melker12345.phonetopc.data.BiometricKey
import io.github.melker12345.phonetopc.data.DecryptKey
import io.github.melker12345.phonetopc.protocol.approval.ApprovalKeyInfo
import io.github.melker12345.phonetopc.protocol.approval.ApprovalOutcome
import io.github.melker12345.phonetopc.protocol.approval.ApprovalRequest
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy
import io.github.melker12345.phonetopc.protocol.approval.DecryptOutcome
import io.github.melker12345.phonetopc.protocol.approval.DecryptRequest
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.withContext
import javax.crypto.BadPaddingException
import javax.crypto.IllegalBlockSizeException

/**
 * The phone's [io.github.melker12345.phonetopc.protocol.approval.Approver]: the Keystore
 * approval and decryption keys, a heads-up notification and a biometric prompt (APPROVAL.md
 * §2, §4.2; DECRYPT.md §2, §4).
 */
class AndroidApprover(context: Context) : io.github.melker12345.phonetopc.protocol.approval.Approver {
    private val app = context.applicationContext

    override fun keyInfo(): ApprovalKeyInfo? = (ApprovalKey.status(app) as? BiometricKey.Status.Available)?.info

    override fun decryptKeyInfo(): ApprovalKeyInfo? = (DecryptKey.status(app) as? BiometricKey.Status.Available)?.info

    override suspend fun approve(request: ApprovalRequest): ApprovalOutcome {
        val signature = try {
            withContext(Dispatchers.IO) { ApprovalKey.signatureForPrompt(app) }
        } catch (e: BiometricKey.KeyReplacedException) {
            // Fingerprints changed: the new key has another pin, which the PC must accept
            // first (APPROVAL.md §3). It sees it in its next hello.
            return ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
        }
        val prompt = ApprovalPrompts.Prompt(request.id, request.pc.name, request.reason, request.timeoutMillis, decrypt = false)
        return when (val result = ApprovalPrompts.run(app, prompt, BiometricPrompt.CryptoObject(signature))) {
            is ApprovalPrompts.Result.Refused -> ApprovalOutcome.Refused(result.why)
            is ApprovalPrompts.Result.Authenticated -> try {
                val sig = result.crypto?.signature ?: error("the prompt returned no CryptoObject")
                sig.update(request.message)
                ApprovalOutcome.Approved(sig.sign())
            } catch (e: Exception) {
                Log.w(TAG, "signing the approval failed", e)
                ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
            }
        }
    }

    override suspend fun decrypt(request: DecryptRequest): DecryptOutcome {
        val cipher = try {
            withContext(Dispatchers.IO) { DecryptKey.cipherForPrompt(app) }
        } catch (e: BiometricKey.KeyReplacedException) {
            return DecryptOutcome.Refused(ApprovalWhy.UNAVAILABLE)
        }
        val prompt = ApprovalPrompts.Prompt(request.id, request.pc.name, request.reason, request.timeoutMillis, decrypt = true)
        return when (val result = ApprovalPrompts.run(app, prompt, BiometricPrompt.CryptoObject(cipher))) {
            is ApprovalPrompts.Result.Refused -> DecryptOutcome.Refused(result.why)
            is ApprovalPrompts.Result.Authenticated -> try {
                val c = result.crypto?.cipher ?: error("the prompt returned no CryptoObject")
                DecryptOutcome.Decrypted(withContext(Dispatchers.IO) { c.doFinal(request.ciphertext) })
            } catch (e: BadPaddingException) {
                DecryptOutcome.Refused(ApprovalWhy.FAILED)
            } catch (e: IllegalBlockSizeException) {
                DecryptOutcome.Refused(ApprovalWhy.FAILED)
            } catch (e: Exception) {
                Log.w(TAG, "decrypting failed", e)
                DecryptOutcome.Refused(ApprovalWhy.UNAVAILABLE)
            }
        }
    }

    private companion object {
        const val TAG = "AndroidApprover"
    }
}

/**
 * The one approval or decryption being shown, shared by the notification, its actions and
 * [ApprovalActivity]. [io.github.melker12345.phonetopc.protocol.approval.ApprovalService] asks
 * for one at a time; those of other PCs wait their turn there (APPROVAL.md §4).
 */
object ApprovalPrompts {
    private const val TAG = "ApprovalPrompts"

    /** What the user sees. [decrypt] picks the wording. */
    data class Prompt(val id: Long, val pcName: String, val reason: String, val timeoutMillis: Long, val decrypt: Boolean)

    sealed interface Result {
        /** The biometric check passed; [crypto] is now authorised for one operation. */
        class Authenticated(val crypto: BiometricPrompt.CryptoObject?) : Result

        data class Refused(val why: ApprovalWhy) : Result
    }

    class Pending(val prompt: Prompt, val crypto: BiometricPrompt.CryptoObject) {
        internal val result = CompletableDeferred<Result>()
    }

    private val _current = MutableStateFlow<Pending?>(null)

    /** The request waiting for the user; `null` once it ended for any reason. */
    val current: StateFlow<Pending?> = _current.asStateFlow()

    /**
     * Shows the request and waits for the user. Cancelling (timeout, session closed) removes
     * the notification, and [ApprovalActivity] closes its prompt when [current] changes.
     *
     * If the app is in the foreground the prompt opens directly (§4.2 MAY). Otherwise a
     * notification is posted; if notifications are blocked there is no way to reach the user
     * from the background, so the answer is `unavailable` right away.
     */
    suspend fun run(context: Context, prompt: Prompt, crypto: BiometricPrompt.CryptoObject): Result {
        val pending = Pending(prompt, crypto)
        _current.value = pending
        try {
            val shown = if (AppVisibility.inForeground) {
                openPrompt(context, prompt.id)
            } else {
                ApprovalNotifications.show(context, prompt)
            }
            if (!shown) return Result.Refused(ApprovalWhy.UNAVAILABLE)
            return pending.result.await()
        } finally {
            _current.compareAndSet(pending, null)
            withContext(NonCancellable) { ApprovalNotifications.cancel(context) }
        }
    }

    /** From the notification's Deny, its dismissal, or the prompt. Ignored if [id] already ended. */
    fun finish(id: Long, why: ApprovalWhy) {
        _current.value?.takeIf { it.prompt.id == id }?.result?.complete(Result.Refused(why))
    }

    /** The biometric check passed, with the prompt's now-authorised [crypto]. */
    fun authenticated(id: Long, crypto: BiometricPrompt.CryptoObject?) {
        _current.value?.takeIf { it.prompt.id == id }?.result?.complete(Result.Authenticated(crypto))
    }

    /** Only allowed while the app is visible. */
    private fun openPrompt(context: Context, id: Long): Boolean = try {
        context.startActivity(ApprovalActivity.intent(context, id).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        true
    } catch (e: Exception) {
        Log.w(TAG, "couldn't open the approval prompt", e)
        false
    }
}
