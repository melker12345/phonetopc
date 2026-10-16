package io.github.melker12345.phonetopc.approval

import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.activity.compose.setContent
import androidx.biometric.BiometricManager.Authenticators.BIOMETRIC_STRONG
import androidx.biometric.BiometricPrompt
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.core.content.ContextCompat
import androidx.fragment.app.FragmentActivity
import androidx.lifecycle.Lifecycle
import androidx.lifecycle.lifecycleScope
import androidx.lifecycle.repeatOnLifecycle
import io.github.melker12345.phonetopc.R
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy
import io.github.melker12345.phonetopc.ui.theme.PhonetopcTheme
import kotlinx.coroutines.launch

/**
 * A transparent screen that hosts the number choice and then the biometric prompt of an approval
 * (APPROVAL.md §4.2). First the user picks the number the PC shows among three; a wrong one
 * answers `mismatch` without a prompt. The prompt: title "<program> on <PC name>", subtitle the
 * reason, negative button "Deny", strong biometrics only (no PIN/pattern fallback). It carries
 * the key's `Signature` or `Cipher` as its CryptoObject, so a successful check is what lets the
 * key sign or decrypt.
 *
 * Deliberately not shown over the lock screen (no `setShowWhenLocked`): opened from a
 * notification on a locked phone, Android asks the user to unlock first, and only then shows
 * this. Someone who picks up a locked phone can't even read what is being approved, and the
 * prompt itself can never be reached without the device being unlocked.
 */
class ApprovalActivity : FragmentActivity() {
    private var requestId = NO_REQUEST
    private var prompt: BiometricPrompt? = null
    private var picked by mutableStateOf(false)
    private var closing by mutableStateOf(false)

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
        // On recreation after the pick, the prompt's fragment survives and reconnects to the new
        // callback; before it, the choice is shown again.
        picked = savedInstanceState?.getBoolean(STATE_PICKED) ?: false
        setContent {
            PhonetopcTheme {
                if (!picked && !closing) {
                    NumberChoice(
                        pending.prompt,
                        onPick = { n ->
                            picked = true
                            if (n == pending.prompt.match) authenticate(biometricPrompt, pending) else finishWith(ApprovalWhy.MISMATCH)
                        },
                        onDeny = { finishWith(ApprovalWhy.DENIED) },
                        onCancel = { finishWith(ApprovalWhy.CANCELLED) },
                    )
                }
            }
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

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putBoolean(STATE_PICKED, picked)
    }

    private fun authenticate(biometricPrompt: BiometricPrompt, pending: ApprovalPrompts.Pending) {
        val p = pending.prompt
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle(getString(if (p.decrypt) R.string.decrypt_prompt_title else R.string.approval_prompt_title, p.program, p.pcName))
            .setSubtitle(p.reason)
            .setNegativeButtonText(getString(R.string.approval_deny))
            .setAllowedAuthenticators(BIOMETRIC_STRONG)
            .setConfirmationRequired(true)
            .build()
        biometricPrompt.authenticate(info, pending.crypto)
    }

    private fun finishWith(why: ApprovalWhy) {
        ApprovalPrompts.finish(requestId, why)
        close()
    }

    private fun close() {
        closing = true
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
        private const val STATE_PICKED = "picked"
        private const val EXTRA_ID = "request_id"
        private const val NO_REQUEST = -1L

        fun intent(context: Context, id: Long): Intent = Intent(context, ApprovalActivity::class.java)
            .putExtra(EXTRA_ID, id)
            // A fresh instance for every request, in its own task, off the recents list.
            .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TASK or Intent.FLAG_ACTIVITY_NO_ANIMATION)
    }
}
