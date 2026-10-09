package io.github.melker12345.phonetopc.approval

import androidx.biometric.BiometricPrompt
import io.github.melker12345.phonetopc.protocol.approval.ApprovalWhy
import org.junit.Assert.assertEquals
import org.junit.Test

/** BiometricPrompt errors → APPROVAL.md §4.1 outcomes. */
class BiometricOutcomesTest {
    @Test fun mapsPromptErrorsToOutcomes() {
        val expected = mapOf(
            BiometricPrompt.ERROR_NEGATIVE_BUTTON to ApprovalWhy.DENIED,
            BiometricPrompt.ERROR_USER_CANCELED to ApprovalWhy.CANCELLED,
            BiometricPrompt.ERROR_CANCELED to ApprovalWhy.CANCELLED,
            BiometricPrompt.ERROR_TIMEOUT to ApprovalWhy.TIMEOUT,
            BiometricPrompt.ERROR_LOCKOUT to ApprovalWhy.LOCKOUT,
            BiometricPrompt.ERROR_LOCKOUT_PERMANENT to ApprovalWhy.LOCKOUT,
            BiometricPrompt.ERROR_NO_BIOMETRICS to ApprovalWhy.UNAVAILABLE,
            BiometricPrompt.ERROR_HW_NOT_PRESENT to ApprovalWhy.UNAVAILABLE,
            BiometricPrompt.ERROR_HW_UNAVAILABLE to ApprovalWhy.UNAVAILABLE,
            BiometricPrompt.ERROR_NO_DEVICE_CREDENTIAL to ApprovalWhy.UNAVAILABLE,
            BiometricPrompt.ERROR_UNABLE_TO_PROCESS to ApprovalWhy.UNAVAILABLE,
            BiometricPrompt.ERROR_VENDOR to ApprovalWhy.UNAVAILABLE,
            9999 to ApprovalWhy.UNAVAILABLE,
        )
        for ((code, why) in expected) assertEquals("error $code", why, BiometricOutcomes.forError(code))
    }
}
