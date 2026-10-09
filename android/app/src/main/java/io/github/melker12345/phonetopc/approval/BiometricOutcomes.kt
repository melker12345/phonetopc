package io.github.melker12345.phonetopc.approval

import androidx.biometric.BiometricPrompt
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy

/** How a `BiometricPrompt` error ends an approval (APPROVAL.md §4.1). */
object BiometricOutcomes {
    fun forError(errorCode: Int): ApprovalWhy = when (errorCode) {
        // The prompt's negative button is labelled "Deny".
        BiometricPrompt.ERROR_NEGATIVE_BUTTON -> ApprovalWhy.DENIED
        // Back, tapping outside, or the system took the prompt away (another app, screen off).
        BiometricPrompt.ERROR_USER_CANCELED, BiometricPrompt.ERROR_CANCELED -> ApprovalWhy.CANCELLED
        // The sensor's own timeout: nobody touched it.
        BiometricPrompt.ERROR_TIMEOUT -> ApprovalWhy.TIMEOUT
        BiometricPrompt.ERROR_LOCKOUT, BiometricPrompt.ERROR_LOCKOUT_PERMANENT -> ApprovalWhy.LOCKOUT
        // ERROR_NO_BIOMETRICS, ERROR_HW_NOT_PRESENT, ERROR_HW_UNAVAILABLE,
        // ERROR_NO_DEVICE_CREDENTIAL, ERROR_SECURITY_UPDATE_REQUIRED, ERROR_UNABLE_TO_PROCESS,
        // ERROR_NO_SPACE, ERROR_VENDOR and anything newer.
        else -> ApprovalWhy.UNAVAILABLE
    }
}
