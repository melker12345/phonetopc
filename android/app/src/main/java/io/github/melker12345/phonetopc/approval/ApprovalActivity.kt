package io.github.melker12345.phonetopc.approval

import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import io.github.melker12345.phonetopc.R
import kotlinx.coroutines.launch

/**
 * A transparent screen that only hosts the biometric prompt of an approval (APPROVAL.md §4.2):
 * title the PC's name, subtitle the reason, negative button "Deny", strong biometrics only
 * (no PIN/pattern fallback). The prompt carries the key's `Signature` or `Cipher` as its
 * CryptoObject, so a successful check is what lets the key sign or decrypt.
 *
 * Deliberately not shown over the lock screen (no `setShowWhenLocked`): opened from a
 * notification on a locked phone, Android asks the user to unlock first, and only then shows
 * this. Someone who picks up a locked phone can't even read what is being approved, and the
 * prompt itself can never be reached without the device being unlocked.
 */
class ApprovalActivity : FragmentActivity() {
    private var requestId = NO_REQUEST
    private var prompt: BiometricPrompt? = null

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        requestId = intent.getLongExtra(EXTRA_ID, NO_REQUEST)
        val pending = ApprovalPrompts.current.value?.takeIf { it.prompt.id == requestId }
        if (pending == null) {
            // The request already ended (answered, timed out, or the PC went away).
            finish()
            return
        }
        val biometricPrompt = BiometricPrompt(this, ContextCompat.getMainExecutor(this), Callback(requestId))
        prompt = biometricPrompt
        // On recreation the prompt's fragment survives and reconnects to the new callback.
        if (savedInstanceState == null) {
            val info = BiometricPrompt.PromptInfo.Builder()
                .setTitle(
                    if (pending.prompt.decrypt) getString(R.string.decrypt_prompt_title, pending.prompt.pcName) else pending.prompt.pcName,
                )
                .setSubtitle(pending.prompt.reason)
                .setNegativeButtonText(getString(R.string.approval_deny))
                .setAllowedAuthenticators(BIOMETRIC_STRONG)
                .setConfirmationRequired(true)
                .build()
            biometricPrompt.authenticate(info, pending.crypto)
        }
        // §4.2: when the request ends for any reason, the prompt goes away.
        lifecycleScope.launch {
            repeatOnLifecycle(Lifecycle.State.CREATED) {
                ApprovalPrompts.current.collect { current ->
                    if (current?.prompt?.id != requestId) close()
                }
            }
        }
    }

    private fun close() {
        prompt?.cancelAuthentication()
        prompt = null
        if (!isFinishing) finish()
        @Suppress("DEPRECATION") // overrideActivityTransition is API 34+
        overridePendingTransition(0, 0)
    }

    private inner class Callback(private val id: Long) : BiometricPrompt.AuthenticationCallback() {
        override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
            ApprovalPrompts.authenticated(id, result.cryptoObject)
            close()
        }

        override fun onAuthenticationError(errorCode: Int, errString: CharSequence) {
            ApprovalPrompts.finish(id, BiometricOutcomes.forError(errorCode))
            close()
        }

        // onAuthenticationFailed: a finger that didn't match. The prompt stays up for another
        // try; too many and Android reports ERROR_LOCKOUT.
    }

    companion object {
        private const val EXTRA_ID = "request_id"
        private const val NO_REQUEST = -1L

        fun intent(context: Context, id: Long): Intent = Intent(context, ApprovalActivity::class.java)
            .putExtra(EXTRA_ID, id)
            // A fresh instance for every request, in its own task, off the recents list.
            .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TASK or Intent.FLAG_ACTIVITY_NO_ANIMATION)
    }
}
