package io.github.melker12345.phonetopc.approval

import android.content.Context
import android.content.Intent
import android.util.Log
import io.github.melker12345.phonetopc.data.ApprovalKey
import io.github.melker12345.phonetopc.protocol.approval.ApprovalKeyInfo
import io.github.melker12345.phonetopc.protocol.approval.ApprovalOutcome
import io.github.melker12345.phonetopc.protocol.approval.ApprovalRequest
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy
import kotlinx.coroutines.CompletableDeferred
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.withContext
import java.security.Signature

/**
 * The phone's [io.github.melker12345.phonetopc.protocol.approval.Approver]: the Keystore
 * approval key, a heads-up notification and a biometric prompt (APPROVAL.md §2, §4.2).
 */
class AndroidApprover(context: Context) : io.github.melker12345.phonetopc.protocol.approval.Approver {
    private val app = context.applicationContext

    override fun keyInfo(): ApprovalKeyInfo? = (ApprovalKey.status(app) as? ApprovalKey.Status.Available)?.info

    override suspend fun approve(request: ApprovalRequest): ApprovalOutcome {
        val signature = try {
            withContext(Dispatchers.IO) { ApprovalKey.signatureForPrompt(app) }
        } catch (e: ApprovalKey.KeyReplacedException) {
            // Fingerprints changed: the new key has another pin, which the PC must accept
            // first (APPROVAL.md §3). It sees it in its next hello.
            return ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
        }
        return ApprovalPrompts.run(app, request, signature)
    }
}

/**
 * The one approval being shown (there is at most one per phone, APPROVAL.md §4), shared by the
 * notification, its actions and [ApprovalActivity].
 */
object ApprovalPrompts {
    private const val TAG = "ApprovalPrompts"

    class Pending(val request: ApprovalRequest, val signature: Signature) {
        internal val outcome = CompletableDeferred<ApprovalOutcome>()
    }

    private val _current = MutableStateFlow<Pending?>(null)

    /** The approval waiting for the user; `null` once it ended for any reason. */
    val current: StateFlow<Pending?> = _current.asStateFlow()

    /**
     * Shows the approval and waits for the user. Cancelling (timeout, session closed) removes
     * the notification, and [ApprovalActivity] closes its prompt when [current] changes.
     *
     * If the app is in the foreground the prompt opens directly (§4.2 MAY). Otherwise a
     * notification is posted; if notifications are blocked there is no way to reach the user
     * from the background, so the answer is `unavailable` right away.
     */
    suspend fun run(context: Context, request: ApprovalRequest, signature: Signature): ApprovalOutcome {
        val pending = Pending(request, signature)
        _current.value = pending
        try {
            val shown = if (AppVisibility.inForeground) {
                openPrompt(context, request.id)
            } else {
                ApprovalNotifications.show(context, request)
            }
            if (!shown) return ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
            return pending.outcome.await()
        } finally {
            _current.compareAndSet(pending, null)
            withContext(NonCancellable) { ApprovalNotifications.cancel(context) }
        }
    }

    /** From the notification's Deny, its dismissal, or the prompt. Ignored if [id] already ended. */
    fun finish(id: Long, outcome: ApprovalOutcome) {
        val pending = _current.value?.takeIf { it.request.id == id } ?: return
        pending.outcome.complete(outcome)
    }

    /**
     * The biometric check passed: sign the message with the now-authorised [signature] (from
     * the prompt's CryptoObject). Called on the main thread; Keystore signing is quick.
     */
    fun signed(id: Long, signature: Signature?) {
        val pending = _current.value?.takeIf { it.request.id == id } ?: return
        val outcome = try {
            val sig = signature ?: error("the prompt returned no CryptoObject")
            sig.update(pending.request.message)
            ApprovalOutcome.Approved(sig.sign())
        } catch (e: Exception) {
            Log.w(TAG, "signing the approval failed", e)
            ApprovalOutcome.Refused(ApprovalWhy.UNAVAILABLE)
        }
        pending.outcome.complete(outcome)
    }

    /** Opens [ApprovalActivity] for [id]. Only allowed while the app is visible. */
    private fun openPrompt(context: Context, id: Long): Boolean = try {
        context.startActivity(ApprovalActivity.intent(context, id).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
        true
    } catch (e: Exception) {
        Log.w(TAG, "couldn't open the approval prompt", e)
        false
    }
}
